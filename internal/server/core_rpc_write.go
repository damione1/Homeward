package server

import "context"

// The common transport owns the writer and its original deadline. Readers
// request only its bounded error grace, without cancelling a credential lease.
type publicWriteInterruptContextKey struct{}

func interruptCoreRPCWriteOnCancel(ctx, operation context.Context) func() {
	interrupt, ok := ctx.Value(publicWriteInterruptContextKey{}).(func())
	if !ok || operation == nil {
		return func() {}
	}
	done := make(chan struct{})
	stop := context.AfterFunc(operation, func() { defer close(done); interrupt() })
	return func() {
		if !stop() {
			<-done
		}
	}
}

func interruptCoreRPCWriteOnChange(ctx context.Context, changes <-chan struct{}) func() {
	interrupt, ok := ctx.Value(publicWriteInterruptContextKey{}).(func())
	if !ok || changes == nil {
		return func() {}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-changes:
			interrupt()
		case <-ctx.Done():
			interrupt()
		case <-stop:
		}
	}()
	return func() { close(stop); <-done }
}
