package lists

import "testing"

func TestNHA162SlowSubscriberMustResyncAndClose(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	sub, err := f.service.SubscribeCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= MaxNotificationQueue; i++ {
		if _, err := f.service.CreateList(ctx, mustList(t, "Synthetic notification")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sub.Next(ctx); !IsCode(err, ResyncRequired) {
		t.Fatalf("slow subscriber failed to become terminal: %v", err)
	}
	sub.Close()
	if _, err := sub.Next(ctx); !IsCode(err, Closed) {
		t.Fatalf("closed subscriber admitted further work: %v", err)
	}
}
