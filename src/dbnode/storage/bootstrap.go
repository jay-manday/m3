// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package storage

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/m3db/m3/src/dbnode/clock"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap"
	xerrors "github.com/m3db/m3/src/x/errors"
	"github.com/m3db/m3/src/x/instrument"

	"github.com/uber-go/tally"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	// errNamespaceIsBootstrapping raised when trying to bootstrap a namespace that's being bootstrapped.
	errNamespaceIsBootstrapping = errors.New("namespace is bootstrapping")

	// errNamespaceNotBootstrapped raised when trying to flush/snapshot data for a namespace that's not yet bootstrapped.
	errNamespaceNotBootstrapped = errors.New("namespace is not yet bootstrapped")

	// errShardIsBootstrapping raised when trying to bootstrap a shard that's being bootstrapped.
	errShardIsBootstrapping = errors.New("shard is bootstrapping")

	// errShardNotBootstrappedToFlush raised when trying to flush data for a shard that's not yet bootstrapped.
	errShardNotBootstrappedToFlush = errors.New("shard is not yet bootstrapped to flush")

	// errShardNotBootstrappedToSnapshot raised when trying to snapshot data for a shard that's not yet bootstrapped.
	errShardNotBootstrappedToSnapshot = errors.New("shard is not yet bootstrapped to snapshot")

	// errShardNotBootstrappedToRead raised when trying to read data for a shard that's not yet bootstrapped.
	errShardNotBootstrappedToRead = errors.New("shard is not yet bootstrapped to read")

	// errIndexNotBootstrappedToRead raised when trying to read the index before being bootstrapped.
	errIndexNotBootstrappedToRead = errors.New("index is not yet bootstrapped to read")

	// errBootstrapEnqueued raised when trying to bootstrap and bootstrap becomes enqueued.
	errBootstrapEnqueued = errors.New("database bootstrapping enqueued bootstrap")
)

type bootstrapManager struct {
	sync.RWMutex

	database                    database
	mediator                    databaseMediator
	opts                        Options
	log                         *zap.Logger
	nowFn                       clock.NowFn
	processProvider             bootstrap.ProcessProvider
	state                       BootstrapState
	hasPending                  bool
	status                      tally.Gauge
	lastBootstrapCompletionTime time.Time
}

func newBootstrapManager(
	database database,
	mediator databaseMediator,
	opts Options,
) databaseBootstrapManager {
	scope := opts.InstrumentOptions().MetricsScope()
	return &bootstrapManager{
		database:        database,
		mediator:        mediator,
		opts:            opts,
		log:             opts.InstrumentOptions().Logger(),
		nowFn:           opts.ClockOptions().NowFn(),
		processProvider: opts.BootstrapProcessProvider(),
		status:          scope.Gauge("bootstrapped"),
	}
}

func (m *bootstrapManager) IsBootstrapped() bool {
	m.RLock()
	state := m.state
	m.RUnlock()
	return state == Bootstrapped
}

func (m *bootstrapManager) LastBootstrapCompletionTime() (time.Time, bool) {
	return m.lastBootstrapCompletionTime, !m.lastBootstrapCompletionTime.IsZero()
}

func (m *bootstrapManager) Bootstrap() error {
	m.Lock()
	switch m.state {
	case Bootstrapping:
		// NB(r): Already bootstrapping, now a consequent bootstrap
		// request comes in - we queue this up to bootstrap again
		// once the current bootstrap has completed.
		// This is an edge case that can occur if during either an
		// initial bootstrap or a resharding bootstrap if a new
		// reshard occurs and we need to bootstrap more shards.
		m.hasPending = true
		m.Unlock()
		return errBootstrapEnqueued
	default:
		m.state = Bootstrapping
	}
	m.Unlock()

	// NB(xichen): disable filesystem manager before we bootstrap to minimize
	// the impact of file operations on bootstrapping performance
	m.mediator.DisableFileOps()
	defer m.mediator.EnableFileOps()

	// Keep performing bootstraps until none pending
	multiErr := xerrors.NewMultiError()
	for {
		err := m.bootstrap()
		if err != nil {
			multiErr = multiErr.Add(err)
		}

		m.Lock()
		currPending := m.hasPending
		if currPending {
			// New bootstrap calls should now enqueue another pending bootstrap
			m.hasPending = false
		} else {
			m.state = Bootstrapped
		}
		m.Unlock()

		if !currPending {
			break
		}
	}

	// NB(xichen): in order for bootstrapped data to be flushed to disk, a tick
	// needs to happen to drain the in-memory buffers and a consequent flush will
	// flush all the data onto disk. However, this has shown to be too intensive
	// to do immediately after bootstrap due to bootstrapping nodes simultaneously
	// attempting to tick through their series and flushing data, adding significant
	// load to the cluster. It turns out to be better to let ticking happen naturally
	// on its own course so that the load of ticking and flushing is more spread out
	// across the cluster.

	m.lastBootstrapCompletionTime = m.nowFn()
	return multiErr.FinalError()
}

func (m *bootstrapManager) Report() {
	if m.IsBootstrapped() {
		m.status.Update(1)
	} else {
		m.status.Update(0)
	}
}

func (m *bootstrapManager) bootstrap() error {
	// NB(r): construct new instance of the bootstrap process to avoid
	// state being kept around by bootstrappers.
	process, err := m.processProvider.Provide()
	if err != nil {
		return err
	}

	namespaces, err := m.database.GetOwnedNamespaces()
	if err != nil {
		return err
	}

	uniqueShards := make(map[uint32]struct{})
	targets := make([]bootstrap.ProcessNamespace, 0, len(namespaces))
	for _, namespace := range namespaces {
		namespaceShards := namespace.GetOwnedShards()
		bootstrapShards := make([]uint32, 0, len(namespaceShards))
		for _, shard := range namespaceShards {
			if shard.IsBootstrapped() {
				continue
			}

			uniqueShards[shard.ID()] = struct{}{}
			bootstrapShards = append(bootstrapShards, shard.ID())
		}

		accumulator := newDatabaseNamespaceDataAccumulator(namespace)

		targets = append(targets, bootstrap.ProcessNamespace{
			Metadata:        namespace.Metadata(),
			Shards:          bootstrapShards,
			DataAccumulator: accumulator,
		})
	}

	start := m.nowFn()
	logFields := []zapcore.Field{
		zap.Int("numShards", len(uniqueShards)),
	}
	m.log.Info("bootstrap started", logFields...)

	// Run the bootstrap.
	bootstrapResult, err := process.Run(start, targets)

	logFields = append(logFields,
		zap.Duration("duration", m.nowFn().Sub(start)))

	if err != nil {
		m.log.Error("bootstrap failed",
			append(logFields, zap.Error(err))...)
		return err
	}

	// Use a multi-error here because we want to at least bootstrap
	// as many of the namespaces as possible.
	multiErr := xerrors.NewMultiError()
	for _, namespace := range namespaces {
		id := namespace.ID()
		result, ok := bootstrapResult.Results.Get(id)
		if !ok {
			err := fmt.Errorf("missing namespace from bootstrap result: %v",
				id.String())
			i := m.opts.InstrumentOptions()
			instrument.EmitAndLogInvariantViolation(i, func(l *zap.Logger) {
				l.Error("bootstrap failed",
					append(logFields, zap.Error(err))...)
			})
			return err
		}

		if err := namespace.Bootstrap(result); err != nil {
			multiErr = multiErr.Add(err)
		}
	}

	if err := multiErr.FinalError(); err != nil {
		m.log.Info("bootstrap namespaces failed",
			append(logFields, zap.Error(err))...)
		return err
	}

	m.log.Info("bootstrap success")
	return nil
}
