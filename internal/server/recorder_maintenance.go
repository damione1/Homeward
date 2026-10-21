package server

import (
	"context"

	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
)

// This adapter borrows the only sequencer and Store owned by Run. Preparation
// creates no goroutine or SQL transaction; Run starts it with its other owners.
type recorderMaintenanceBoundary struct{ sequence *sequencer.Sequencer }

func (b recorderMaintenanceBoundary) Boundary() (recordermaintenance.Boundary, error) {
	if b.sequence == nil {
		return recordermaintenance.Boundary{}, recordermaintenance.ErrInvalid
	}
	capture, err := b.sequence.CaptureMaintenanceBoundary()
	if err != nil {
		return recordermaintenance.Boundary{}, err
	}
	cursor, at, tick := capture.Components()
	epoch, sequence, err := cursor.Components()
	if err != nil {
		return recordermaintenance.Boundary{}, err
	}
	return recordermaintenance.NewBoundary(epoch, sequence, at, tick)
}
func prepareRecorderMaintenance(sequence *sequencer.Sequencer, store *storage.Store) (*recordermaintenance.Worker, error) {
	if sequence == nil || store == nil {
		return nil, recordermaintenance.ErrInvalid
	}
	return recordermaintenance.New(store, recorderMaintenanceBoundary{sequence})
}
func prepareRecorderMaintenanceWithClock(sequence *sequencer.Sequencer, store *storage.Store, clock recordermaintenance.Clock) (*recordermaintenance.Worker, error) {
	if sequence == nil || store == nil {
		return nil, recordermaintenance.ErrInvalid
	}
	if clock == nil {
		clock = recordermaintenance.RealClock{}
	}
	return recordermaintenance.NewWithClock(store, recorderMaintenanceBoundary{sequence}, clock)
}
func drainRecorderMaintenance(ctx context.Context, worker *recordermaintenance.Worker) error {
	return worker.Close(ctx)
}
