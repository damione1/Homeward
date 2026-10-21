package automationruns

import (
	"context"
	"testing"
	"time"
)

// Real image refs and exact pool accounting prove supersession returns B
// before C is admitted. There is no observation hold or off-pool trace copy.
func TestNHA029AdmissionTransferReturnsCandidateBeforeNextUnion(t *testing.T) {
	c, fresh := nha029ImageFixture(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	a := c.active
	c.mu.Unlock()
	c.pool.mu.Lock()
	initial := c.pool.used
	c.pool.mu.Unlock()
	planB, interpreterB := fresh()
	b, err := c.ReserveImage(planB, interpreterB, interpreterB)
	if err != nil {
		t.Fatal(err)
	}
	chargeB := b.image.charge
	if err := b.PauseAdmissions(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	gate := c.admission
	if gate == nil || gate.charge != imageAdmissionCharge {
		t.Fatal("admission gate has no actual reserved charge")
	}
	c.pool.mu.Lock()
	withB := c.pool.used
	c.pool.mu.Unlock()
	if withB != initial+chargeB+imageAdmissionCharge {
		t.Fatalf("unaccounted candidate/gate storage: initial=%d candidate=%d actual=%d", initial, chargeB, withB)
	}
	imageB := b.image
	if err := b.ReleaseForReplacement(); err != nil {
		t.Fatal(err)
	}
	c.pool.mu.Lock()
	afterB := c.pool.used
	c.pool.mu.Unlock()
	if afterB != initial+imageAdmissionCharge || imageB.refs != 0 || c.active != a || c.pending != nil || c.admission != gate {
		t.Fatalf("B retained charge or opened A: pool=%d want=%d refs=%d", afterB, initial+imageAdmissionCharge, imageB.refs)
	}
	if traces, _ := interpreterB.NodeTraces(); len(traces) != 0 || interpreterB.image != nil || interpreterB.automations != nil || interpreterB.scripts != nil {
		t.Fatal("released B retained a native image")
	}
	if err := b.ReleaseForReplacement(); err == nil {
		t.Fatal("B was released twice")
	}
	planC, interpreterC := fresh()
	next, err := c.ReserveImage(planC, interpreterC, interpreterC)
	if err != nil {
		t.Fatal(err)
	}
	if next.admission != gate || c.admission != gate {
		t.Fatal("C replaced the existing admission owner/channel")
	}
	if err := next.PauseAdmissions(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := next.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := next.ResumeAdmissions(); err != nil {
		t.Fatal(err)
	}
	c.pool.mu.Lock()
	published := c.pool.used
	c.pool.mu.Unlock()
	if published != controllerCharge+next.image.charge || gate.charge != 0 || c.admission != nil || a.refs != 0 {
		t.Fatalf("published union retained obsolete storage: pool=%d target=%d", published, controllerCharge+next.image.charge)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	c.pool.mu.Lock()
	closed := c.pool.used
	c.pool.mu.Unlock()
	if closed != controllerCharge {
		t.Fatalf("Close retained image/gate charge: %d", closed)
	}
}
