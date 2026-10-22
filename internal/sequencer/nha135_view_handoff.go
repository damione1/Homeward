package sequencer

// CloseForViewHandoff detaches a view only while both its previous stream and
// its already attached successor still prove continuity. Admission, loss and
// closure use the same mutex, so no producer can invalidate either proof
// between checking it and closing the previous mailbox. Refusal changes
// neither subscription; the previous owner must report/reconcile its loss.
// The caller separately owns the captured image and suffix for next; this
// operation attests only live-mailbox continuity at detachment.
func (sub *Subscription) CloseForViewHandoff(next *Subscription) error {
	if sub == nil || sub.owner == nil || next == nil || next == sub || next.owner != sub.owner || sub.class != SubscriptionView || next.class != SubscriptionView {
		return ErrInvalidInput
	}
	sub.owner.mu.Lock()
	defer sub.owner.mu.Unlock()
	if sub.closed || next.closed {
		return ErrSubscriptionClosed
	}
	if sub.loss != nil || !sub.replayValid || sub.degraded || next.loss != nil || !next.replayValid || next.degraded {
		return ErrCursorInvalidated
	}
	sub.closed = true
	sub.queue = nil
	sub.queueBytes = 0
	delete(sub.owner.subs, sub)
	sub.signal()
	return nil
}
