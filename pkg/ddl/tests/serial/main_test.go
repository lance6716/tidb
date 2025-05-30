// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Copyright 2013 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

package serial

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/domain/infosync"
	"github.com/pingcap/tidb/pkg/keyspace"
	"github.com/pingcap/tidb/pkg/meta/autoid"
	"github.com/pingcap/tidb/pkg/testkit/testsetup"
	"github.com/tikv/client-go/v2/tikv"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	testsetup.SetupForCommonTest()
	tikv.EnableFailpoints()

	domain.SchemaOutOfDateRetryInterval.Store(50 * time.Millisecond)
	domain.SchemaOutOfDateRetryTimes.Store(50)

	autoid.SetStep(5000)
	ddl.CheckBackfillJobFinishInterval = 50 * time.Millisecond
	ddl.RunInGoTest = true
	ddl.SetBatchInsertDeleteRangeSize(2)

	config.UpdateGlobal(func(conf *config.Config) {
		// Test for table lock.
		conf.EnableTableLock = true
		conf.Instance.SlowThreshold = 10000
		conf.TiKVClient.AsyncCommit.SafeWindow = 0
		conf.TiKVClient.AsyncCommit.AllowedClockDrift = 0
		conf.Experimental.AllowsExpressionIndex = true
	})

	_, err := infosync.GlobalInfoSyncerInit(context.Background(), "t", func() uint64 { return 1 }, nil, nil, nil, nil, keyspace.CodecV1, true, nil)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ddl: infosync.GlobalInfoSyncerInit: %v\n", err)
		os.Exit(1)
	}

	opts := []goleak.Option{
		goleak.IgnoreTopFunction("github.com/golang/glog.(*fileSink).flushDaemon"),
		goleak.IgnoreTopFunction("github.com/bazelbuild/rules_go/go/tools/bzltestutil.RegisterTimeoutHandler.func1"),
		goleak.IgnoreTopFunction("github.com/lestrrat-go/httprc.runFetchWorker"),
		goleak.IgnoreTopFunction("go.etcd.io/etcd/client/pkg/v3/logutil.(*MergeLogger).outputLoop"),
		goleak.IgnoreTopFunction("github.com/tikv/client-go/v2/txnkv/transaction.keepAlive"),
		goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
	}

	goleak.VerifyTestMain(m, opts...)
}
