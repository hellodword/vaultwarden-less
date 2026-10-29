package worker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

const operationLimit = 100

type OperationTarget struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	Error      string    `json:"error,omitempty"`
	ExitCode   int       `json:"exit_code"`
	RetryAt    time.Time `json:"retry_at,omitzero"`
}
type Operation struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Force    bool              `json:"force,omitempty"`
	Target   string            `json:"target,omitempty"`
	ReadData bool              `json:"read_data,omitempty"`
	State    string            `json:"state"`
	Created  time.Time         `json:"created"`
	Started  time.Time         `json:"started,omitzero"`
	Finished time.Time         `json:"finished,omitzero"`
	Source   string            `json:"source,omitempty"`
	Error    string            `json:"error,omitempty"`
	Targets  []OperationTarget `json:"targets"`
}

func (o Operation) clone() Operation { o.Targets = slices.Clone(o.Targets); return o }
func (o Operation) Done() bool       { return o.State == "succeeded" || o.State == "failed" }
func (w *Worker) newOperation(kind string) (*Operation, error) {
	// Keep all accepted live jobs and a bounded recent history. Reject overload
	// instead of silently losing an accepted receipt.
	for len(w.order) >= operationLimit {
		index := slices.IndexFunc(w.order, func(id string) bool { return w.operations[id].Done() })
		if index < 0 {
			return nil, errors.New("operations_busy")
		}
		delete(w.operations, w.order[index])
		w.order = slices.Delete(w.order, index, index+1)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, errors.New("operation_id_failed")
	}
	op := &Operation{ID: hex.EncodeToString(id[:]), Kind: kind, State: "queued", Created: w.clock.Now(), Targets: []OperationTarget{}}
	w.operations[op.ID] = op
	w.order = append(w.order, op.ID)
	return op, nil
}
func (w *Worker) Backup(force bool) (Operation, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	op, err := w.newOperation("backup")
	if err != nil {
		return Operation{}, err
	}
	op.Force = force
	for _, t := range w.targets {
		op.Targets = append(op.Targets, OperationTarget{Name: t.config.Name, State: "queued"})
	}
	w.schedule.manual()
	w.schedule.pending.Operations = append(w.schedule.pending.Operations, op.ID)
	w.signal()
	return op.clone(), nil
}
func (w *Worker) Verify(name string, readData bool) (Operation, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var target *targetState
	for _, t := range w.targets {
		if t.config.Name == name {
			target = t
			break
		}
	}
	if target == nil {
		return Operation{}, errors.New("unknown_target")
	}
	op, err := w.newOperation("verify")
	if err != nil {
		return Operation{}, err
	}
	op.Target = name
	op.ReadData = readData
	op.Targets = append(op.Targets, OperationTarget{Name: name, State: "queued"})
	if target.config.Error != "" {
		w.failOperation(op.ID, target.config.Error)
	} else {
		target.verifyQueue = append(target.verifyQueue, op.ID)
	}
	w.signal()
	return op.clone(), nil
}
func (w *Worker) Operation(id string) (Operation, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	op, ok := w.operations[id]
	if !ok {
		return Operation{}, false
	}
	return op.clone(), true
}
func (w *Worker) failOperation(id, code string) {
	op := w.operations[id]
	op.State = "failed"
	op.Error = code
	op.Finished = w.clock.Now()
	for i := range op.Targets {
		op.Targets[i].State = "failed"
		op.Targets[i].Error = code
	}
}
func (w *Worker) targetOperation(id string, t *targetState, outcome OperationTarget) {
	op := w.operations[id]
	outcome.Name = t.config.Name
	for i := range op.Targets {
		if op.Targets[i].Name == outcome.Name {
			op.Targets[i] = outcome
		}
	}
	done, success := true, op.Source != "incomplete" && op.Source != "failed"
	for _, r := range op.Targets {
		switch r.State {
		case "uploaded", "unchanged", "verified":
		case "failed", "deferred":
			success = false
		default:
			done = false
		}
	}
	if done {
		op.State = "succeeded"
		if !success {
			op.State = "failed"
		}
		op.Finished = w.clock.Now()
	}
}
func (w *Worker) deferDemands(t *targetState) {
	retained := t.demands[:0]
	for _, id := range t.demands {
		if w.operations[id].Force {
			retained = append(retained, id)
			continue
		}
		w.targetOperation(id, t, OperationTarget{State: "deferred", Error: t.status.Error, ExitCode: t.status.ExitCode, RetryAt: t.status.RetryAt})
	}
	t.demands = retained
}
