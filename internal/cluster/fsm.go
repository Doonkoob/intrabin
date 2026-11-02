package cluster

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/storage"
)

const appliedIndexKey = "local.raft_applied_index"

// fsm applies committed control-plane commands to the local SQLite database.
//
// Commands are state-based and carry their own timestamps, so execution is deterministic. The
// last applied index is persisted so that a restart without a snapshot does not re-execute
// entries that are already reflected in the database.
type fsm struct {
	x       *controlplane.Executor
	st      *storage.Store
	log     *slog.Logger
	applied atomic.Uint64 // persisted applied index at start (skip entries ≤ this)
}

// applyResult is returned from Apply through raft.ApplyFuture.Response().
type applyResult struct{ Err error }

func newFSM(x *controlplane.Executor, st *storage.Store, log *slog.Logger) *fsm {
	f := &fsm{x: x, st: st, log: log}
	if v, err := st.GetSetting(context.Background(), appliedIndexKey); err == nil {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			f.applied.Store(n)
		}
	}
	return f
}

func (f *fsm) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	if l.Index <= f.applied.Load() {
		return applyResult{}
	}
	cmd, err := controlplane.Decode(l.Data)
	if err != nil {
		f.log.Error("fsm: undecodable log entry", "index", l.Index, "err", err)
		return applyResult{Err: err}
	}
	err = f.x.Execute(context.Background(), cmd)
	if err != nil {
		f.log.Debug("fsm: command returned error", "op", cmd.Op, "index", l.Index, "err", err)
	}
	if e := f.st.SetSetting(context.Background(), appliedIndexKey, strconv.FormatUint(l.Index, 10)); e != nil {
		f.log.Error("fsm: persist applied index", "err", e)
	}
	return applyResult{Err: err}
}

func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	data, err := f.st.DumpReplicated(storage.WithNow(context.Background(), time.Now()))
	if err != nil {
		return nil, err
	}
	return &fsmSnapshot{data: data}, nil
}

func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if err := f.st.RestoreReplicated(context.Background(), data); err != nil {
		return err
	}
	// After a restore, raft replays entries after the snapshot index; none may be skipped.
	f.applied.Store(0)
	_ = f.st.SetSetting(context.Background(), appliedIndexKey, "0")
	f.x.NotifyExternal("state.restore")
	return nil
}

type fsmSnapshot struct{ data []byte }

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
