// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Ben Darnell

package storage

import (
	"fmt"
	"sync"

	"github.com/cockroachdb/cockroach/base"
	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/tracing"
	"github.com/opentracing/opentracing-go"
	"golang.org/x/net/context"
)

// intentResolver manages the process of pushing transactions and
// resolving intents.
type intentResolver struct {
	store *Store
}

func newIntentResolver(store *Store) *intentResolver {
	return &intentResolver{store}
}

// processWriteIntentError tries to push the conflicting
// transaction(s) responsible for the given WriteIntentError, and to
// resolve those intents if possible. Returns a new error to be used
// in place of the original.
//
// The returned error may be a copy of the original WriteIntentError,
// with or without the Resolved flag set, which governs the client's
// retry behavior. (if the transaction is pushed, the Resolved flag is
// set to tell the client to retry immediately; otherwise it is false
// to cause the client to back off).
func (ir *intentResolver) processWriteIntentError(ctx context.Context, wiErr roachpb.WriteIntentError, r *Replica, args roachpb.Request, h roachpb.Header, pushType roachpb.PushTxnType) *roachpb.Error {
	if log.V(6) {
		log.Infoc(ctx, "resolving write intent %s", wiErr)
	}

	method := args.Method()
	readOnly := roachpb.IsReadOnly(args) // TODO(tschottdorf): pass as param

	resolveIntents, pushErr := ir.maybePushTransactions(ctx, wiErr.Intents, h, pushType)
	if resErr := ir.resolveIntents(ctx, r, resolveIntents, false /* !wait */, true /* poison */); resErr != nil {
		// When resolving without waiting, errors should not
		// usually be returned here, although there are some cases
		// when they may be (especially when a test cluster is in
		// the process of shutting down).
		log.Warningf("asynchronous resolveIntents failed: %s", resErr)
	}

	if pushErr != nil {
		if log.V(1) {
			log.Infoc(ctx, "on %s: %s", method, pushErr)
		}

		// For write/write conflicts within a transaction, propagate the
		// push failure, not the original write intent error. The push
		// failure will instruct the client to restart the transaction
		// with a backoff.
		if h.Txn != nil && h.Txn.ID != nil && !readOnly {
			return pushErr
		}

		// For read/write conflicts, and non-transactional write/write
		// conflicts, return the write intent error which engages
		// backoff/retry (with !Resolved). We don't need to restart the
		// txn, only resend the read with a backoff.
		return roachpb.NewError(&wiErr)
	}

	// We pushed all transactions, so tell the client everything's
	// resolved and it can retry immediately.
	wiErr.Resolved = true
	return roachpb.NewError(&wiErr)
}

// maybePushTransaction tries to push the conflicting transaction(s)
// responsible for the given intents: either move its
// timestamp forward on a read/write conflict, abort it on a
// write/write conflict, or do nothing if the transaction is no longer
// pending.
//
// Returns a slice of intents which can now be resolved, and an error.
// The returned intents should be resolved via
// intentResolver.resolveIntents regardless of any error returned by
// maybePushTransaction, but if the error is non-nil then some of the
// conflicting transactions may still be pending.
//
// Callers are involved with
// a) conflict resolution for commands being executed at the Store with the
//    client waiting,
// b) resolving intents encountered during inconsistent operations, and
// c) resolving intents upon EndTransaction which are not local to the given
//    range. This is the only path in which the transaction is going to be
//    in non-pending state and doesn't require a push.
func (ir *intentResolver) maybePushTransactions(ctx context.Context, intents []roachpb.Intent, h roachpb.Header, pushType roachpb.PushTxnType) ([]roachpb.Intent, *roachpb.Error) {
	pusherTxn := h.Txn

	sp, cleanupSp := tracing.SpanFromContext(opStore, ir.store.Tracer(), ctx)
	defer cleanupSp()
	sp.LogEvent("intent resolution")

	// Split intents into those we need to push and those which are good to
	// resolve.
	// TODO(tschottdorf): can optimize this and use same underlying slice.
	var pushIntents, resolveIntents []roachpb.Intent
	for _, intent := range intents {
		// The current intent does not need conflict resolution.
		if intent.Status != roachpb.PENDING {
			resolveIntents = append(resolveIntents, intent)
		} else {
			pushIntents = append(pushIntents, intent)
		}
	}

	// Attempt to push the transaction(s) which created the conflicting intent(s).
	now := ir.store.Clock().Now()

	// TODO(tschottdorf): need deduplication here (many pushes for the same
	// txn are awkward but even worse, could ratchet up the priority).
	// If there's no pusher, we communicate a priority by sending an empty
	// txn with only the priority set.
	if pusherTxn == nil {
		pusherTxn = &roachpb.Transaction{
			Priority: roachpb.MakePriority(h.UserPriority),
		}
	}
	var pushReqs []roachpb.Request
	for _, intent := range pushIntents {
		pushReqs = append(pushReqs, &roachpb.PushTxnRequest{
			Span: roachpb.Span{
				Key: intent.Txn.Key,
			},
			PusherTxn: *pusherTxn,
			PusheeTxn: intent.Txn,
			PushTo:    h.Timestamp,
			// The timestamp is used by PushTxn for figuring out whether the
			// transaction is abandoned. If we used the argument's timestamp
			// here, we would run into busy loops because that timestamp
			// usually stays fixed among retries, so it will never realize
			// that a transaction has timed out. See #877.
			Now:      now,
			PushType: pushType,
		})
	}
	// TODO(kaneda): Set the transaction in the header so that the
	// txn is correctly propagated in an error response.
	b := &client.Batch{}
	b.InternalAddRequest(pushReqs...)
	br, err := ir.store.db.RunWithResponse(b)
	if err != nil {
		// TODO(bdarnell): return resolveIntents even on error.
		return nil, err
	}

	for i, intent := range pushIntents {
		pushee := br.Responses[i].GetInner().(*roachpb.PushTxnResponse).PusheeTxn
		intent.Txn = pushee.TxnMeta
		intent.Status = pushee.Status
		resolveIntents = append(resolveIntents, intent)
	}
	return resolveIntents, nil
}

// processIntentsAsync asynchronously processes intents which were
// encountered during another command but did not interfere with the
// execution of that command. This occurs in two cases: inconsistent
// reads and EndTransaction (which queues its own intents for
// processing via this method).
func (ir *intentResolver) processIntentsAsync(r *Replica, intents []intentsWithArg) {
	if len(intents) == 0 {
		return
	}
	now := r.store.Clock().Now()
	ctx := r.context()
	stopper := r.store.Stopper()

	for _, item := range intents {
		// TODO(tschottdorf): avoid data race related to batch unrolling in ExecuteCmd;
		// can probably go again when that provisional code there is gone. Should
		// still be careful though, a retry could happen and race with args.
		args := util.CloneProto(item.args).(roachpb.Request)
		stopper.RunAsyncTask(func() {
			// Everything here is best effort; give up rather than waiting
			// too long (helps avoid deadlocks during test shutdown,
			// although this is imperfect due to the use of an
			// uninterruptible WaitGroup.Wait in beginCmds).
			ctxWithTimeout, cancel := context.WithTimeout(ctx, base.NetworkTimeout)
			defer cancel()
			h := roachpb.Header{Timestamp: now}
			resolveIntents, pushErr := ir.maybePushTransactions(ctxWithTimeout,
				item.intents, h, roachpb.PUSH_TOUCH)
			if pErr := ir.resolveIntents(ctxWithTimeout, r, resolveIntents, true /* wait */, false /* TODO(tschottdorf): #5088 */); pErr != nil {
				log.Warningc(ctxWithTimeout, "failed to resolve intents: %s", pErr)
				return
			}
			if pushErr != nil {
				log.Warningc(ctxWithTimeout, "failed to push during intent resolution: %s", pushErr)
				return
			}
			// We successfully resolved the intents, so we're able to GC from
			// the txn span directly. Note that the sequence cache was cleared
			// out synchronously with EndTransaction (see comments within for
			// an explanation of why that is kosher).
			//
			// Note that we poisoned the sequence caches on the external ranges
			// above. This may seem counter-intuitive, but it's actually
			// necessary: Assume a transaction has committed here, with two
			// external intents, and assume that we did not poison. Normally,
			// these two intents would be resolved in the same batch, but that
			// is not guaranteed (for example, if DistSender has a stale
			// descriptor after a Merge). When resolved separately, the first
			// ResolveIntent would clear out the sequence cache; an individual
			// write on the second (still present) intent could then be
			// replayed and would resolve to a real value (at least for a
			// window of time unless we delete the local txn entry). That's not
			// OK for non-idempotent commands such as Increment.
			// TODO(tschottdorf): We should have another side effect on
			// MVCCResolveIntent (on commit/abort): If it were able to remove
			// the txn from its corresponding entries in the timestamp cache,
			// no more replays at the same timestamp would be possible. This
			// appears to be a useful performance optimization; we could then
			// not poison on EndTransaction. In fact, the above mechanism
			// could be an effective alternative to sequence-cache based
			// poisoning (or the whole sequence cache?) itself.
			//
			// TODO(tschottdorf): down the road, can probably unclog the system
			// here by batching up a bunch of those GCRequests before proposing.
			if args.Method() == roachpb.EndTransaction {
				var ba roachpb.BatchRequest
				txn := item.intents[0].Txn
				gcArgs := roachpb.GCRequest{
					Span: roachpb.Span{
						Key:    r.Desc().StartKey.AsRawKey(),
						EndKey: r.Desc().EndKey.AsRawKey(),
					},
				}
				gcArgs.Keys = append(gcArgs.Keys, roachpb.GCRequest_GCKey{Key: keys.TransactionKey(txn.Key, txn.ID)})

				ba.Add(&gcArgs)
				if _, pErr := r.addWriteCmd(ctxWithTimeout, ba, nil /* nil */); pErr != nil {
					log.Warningf("could not GC completed transaction: %s", pErr)
				}
			}
		})
	}
}

// resolveIntents resolves the given intents. For those which are
// local to the range, we submit directly to the local Raft instance;
// all non-local intents are resolved asynchronously in a batch. If
// `wait` is true, all operations are carried out synchronously and an
// error is returned. Otherwise, the call returns without error as
// soon as all local resolve commands have been **proposed** (not
// executed). This ensures that if a waiting client retries
// immediately after calling this function, it will not hit the same
// intents again.
func (ir *intentResolver) resolveIntents(ctx context.Context, r *Replica, intents []roachpb.Intent, wait bool, poison bool) *roachpb.Error {
	sp, cleanupSp := tracing.SpanFromContext(opReplica, ir.store.Tracer(), ctx)
	defer cleanupSp()

	ctx = opentracing.ContextWithSpan(ctx, nil) // we're doing async stuff below; those need new traces
	sp.LogEvent(fmt.Sprintf("resolving intents [wait=%t]", wait))

	var reqsRemote []roachpb.Request
	baLocal := roachpb.BatchRequest{}
	for i := range intents {
		intent := intents[i] // avoids a race in `i, intent := range ...`
		var resolveArgs roachpb.Request
		var local bool // whether this intent lives on this Range
		{
			if len(intent.EndKey) == 0 {
				resolveArgs = &roachpb.ResolveIntentRequest{
					Span:      intent.Span,
					IntentTxn: intent.Txn,
					Status:    intent.Status,
					Poison:    poison,
				}
				local = r.ContainsKey(intent.Key)
			} else {
				resolveArgs = &roachpb.ResolveIntentRangeRequest{
					Span:      intent.Span,
					IntentTxn: intent.Txn,
					Status:    intent.Status,
					Poison:    poison,
				}
				local = r.ContainsKeyRange(intent.Key, intent.EndKey)
			}
		}

		// If the intent isn't (completely) local, we'll need to send an external request.
		// We'll batch them all up and send at the end.
		if local {
			baLocal.Add(resolveArgs)
		} else {
			reqsRemote = append(reqsRemote, resolveArgs)
		}
	}

	// The local batch goes directly to Raft.
	var wg sync.WaitGroup
	if len(baLocal.Requests) > 0 {
		action := func() *roachpb.Error {
			// Trace this under the ID of the intent owner.
			sp := r.store.Tracer().StartSpan("resolve intents")
			defer sp.Finish()
			ctx = opentracing.ContextWithSpan(ctx, sp)
			// Always operate with a timeout when resolving intents: this
			// prevents rare shutdown timeouts in tests.
			ctxWithTimeout, cancel := context.WithTimeout(ctx, base.NetworkTimeout)
			defer cancel()
			_, pErr := r.addWriteCmd(ctxWithTimeout, baLocal, &wg)
			return pErr
		}
		wg.Add(1)
		if wait || !r.store.Stopper().RunAsyncTask(func() {
			if err := action(); err != nil {
				log.Warningf("unable to resolve local intents; %s", err)
			}
		}) {
			// Still run the task when draining. Our caller already has a task and
			// going async here again is merely for performance, but some intents
			// need to be resolved because they might block other tasks. See #1684.
			// Note that handleSkippedIntents has a TODO in case #1684 comes back.
			if err := action(); err != nil {
				return err
			}
		}
	}

	// Resolve all of the intents which aren't local to the Range.
	if len(reqsRemote) > 0 {
		b := &client.Batch{}
		b.InternalAddRequest(reqsRemote...)
		action := func() *roachpb.Error {
			// TODO(tschottdorf): no tracing here yet.
			return r.store.DB().Run(b)
		}
		if wait || !r.store.Stopper().RunAsyncTask(func() {
			if err := action(); err != nil {
				log.Warningf("unable to resolve external intents: %s", err)
			}
		}) {
			// As with local intents, try async to not keep the caller waiting, but
			// when draining just go ahead and do it synchronously. See #1684.
			if err := action(); err != nil {
				return err
			}
		}
	}

	// Wait until the local ResolveIntents batch has been submitted to
	// raft. No-op if all were non-local.
	wg.Wait()
	return nil
}
