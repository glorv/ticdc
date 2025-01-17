// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package syncer

import (
	"fmt"
	"strings"
	"sync"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"go.uber.org/zap"

	"github.com/pingcap/tiflow/dm/pkg/binlog"
	"github.com/pingcap/tiflow/dm/pkg/log"
)

type locationRecorder struct {
	//            +-------------+
	//        ... |current event| ...
	//       ^    +-------------+    ^
	//       |                       |
	// curStartLocation        curEndLocation
	// there may be more events between curStartLocation and curEndLocation due to the limitation of binlog or
	// implementation of DM, but those events should always belong to one transaction.
	//
	// curStartLocation is used when
	// - display a meaningful location
	// - match the injected location by handle-error
	// - save table checkpoint of DML
	// curEndLocation is used when
	// - handle end location of DDL, when save table checkpoint or shard-resync
	curStartLocation binlog.Location
	curEndLocation   binlog.Location

	// txnEndLocation is the end location of last transaction. If current event is the last event of a txn,
	// txnEndLocation will be assigned from curEndLocation
	// it is used when
	// - reset binlog replication for a finer granularity
	// - save global checkpoint
	txnEndLocation binlog.Location

	// DML will also generate a query event if user set session binlog_format='statement', we use this field to
	// distinguish DML query event.
	inDML bool

	mu sync.Mutex // guard curEndLocation because Syncer.printStatus is reading it from another goroutine.
}

func (l *locationRecorder) reset(loc binlog.Location) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.curStartLocation = loc
	l.curEndLocation = loc
	l.txnEndLocation = loc
}

//nolint:unused
func (l *locationRecorder) getCurEndLocation() binlog.Location {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.curEndLocation
}

//nolint:unused
func (l *locationRecorder) setCurEndLocation(location binlog.Location) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.curEndLocation = location
}

func (l *locationRecorder) saveTxnEndLocation() {
	l.txnEndLocation = l.curEndLocation.Clone()
}

// shouldUpdatePos returns true when the given event is from a real upstream writing, returns false when the event is
// header, heartbeat, etc.
func shouldUpdatePos(e *replication.BinlogEvent) bool {
	switch e.Header.EventType {
	case replication.FORMAT_DESCRIPTION_EVENT, replication.HEARTBEAT_EVENT, replication.IGNORABLE_EVENT,
		replication.PREVIOUS_GTIDS_EVENT, replication.MARIADB_GTID_LIST_EVENT:
		return false
	}
	//nolint:gosimple
	if e.Header.Flags&replication.LOG_EVENT_ARTIFICIAL_F != 0 {
		// ignore events with LOG_EVENT_ARTIFICIAL_F flag(0x0020) set
		// ref: https://dev.mysql.com/doc/internals/en/binlog-event-flag.html
		return false
	}

	return true
}

func (l *locationRecorder) setCurrentGTID(gset mysql.GTIDSet) {
	err := l.curEndLocation.SetGTID(gset)
	if err != nil {
		log.L().DPanic("failed to set GTID set",
			zap.Any("GTID set", gset),
			zap.Error(err))
	}
}

// update maintains the member of locationRecorder as their definitions.
// - curStartLocation is assigned to curEndLocation
// - curEndLocation is tried to be updated in-place
// - txnEndLocation is assigned to curEndLocation when `e` is the last event of a transaction.
func (l *locationRecorder) update(e *replication.BinlogEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.curStartLocation = l.curEndLocation

	if !shouldUpdatePos(e) {
		return
	}

	if event, ok := e.Event.(*replication.RotateEvent); ok {
		nextName := string(event.NextLogName)
		if l.curEndLocation.Position.Name != nextName {
			l.curEndLocation.Position.Name = nextName
			l.curEndLocation.Position.Pos = binlog.FileHeaderLen
			l.saveTxnEndLocation()
		}
		return
	}

	l.curEndLocation.Position.Pos = e.Header.LogPos

	switch ev := e.Event.(type) {
	case *replication.XIDEvent:
		// for transactional engines like InnoDB, COMMIT is xid event
		l.setCurrentGTID(ev.GSet)
		l.saveTxnEndLocation()
		l.inDML = false
	case *replication.QueryEvent:
		query := strings.TrimSpace(string(ev.Query))
		switch query {
		case "BEGIN":
			// MySQL will write a "BEGIN" query event when it starts a DML transaction, we use this event to distinguish
			// DML query event which comes from a session binlog_format = STATEMENT.
			// But MariaDB will not write "BEGIN" query event, we simply hope user should not do that.
			l.inDML = true
		case "COMMIT":
			// for non-transactional engines like MyISAM, COMMIT is query event
			l.inDML = false
		}

		if l.inDML {
			return
		}

		l.setCurrentGTID(ev.GSet)
		l.saveTxnEndLocation()
	case *replication.MariadbGTIDEvent:
		if !ev.IsDDL() {
			l.inDML = true
		}
	}
}

// String implements fmt.Stringer.
func (l *locationRecorder) String() string {
	return fmt.Sprintf("curStartLocation: %s, curEndLocation: %s, txnEndLocation: %s",
		l.curStartLocation.String(), l.curEndLocation.String(), l.txnEndLocation.String())
}
