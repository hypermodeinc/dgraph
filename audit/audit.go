/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package audit

import (
	"fmt"
	"math"
	"os"
	"sync/atomic"

	"github.com/golang/glog"

	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	defaultAuditFilenameF = "%s_audit_%d_%d.log"
	NodeTypeAlpha         = "alpha"
	NodeTypeZero          = "zero"
)

var auditEnabled atomic.Uint32

type AuditEvent struct {
	User        string
	Namespace   uint64
	ServerHost  string
	ClientHost  string
	Endpoint    string
	ReqType     string
	Req         string
	Status      string
	QueryParams map[string][]string
}

const (
	UnauthorisedUser = "UnauthorisedUser"
	UnknownUser      = "UnknownUser"
	UnknownNamespace = math.MaxUint64
	PoorManAuth      = "PoorManAuth"
	Grpc             = "Grpc"
	Http             = "Http"
	WebSocket        = "Websocket"
)

var auditor = &auditLogger{}

type auditLogger struct {
	log *x.Logger
}

func GetAuditConf(conf string) *x.LoggerConf {
	if conf == "" || conf == worker.AuditDefaults {
		return nil
	}
	auditFlag := z.NewSuperFlag(conf).MergeAndCheckDefault(worker.AuditDefaults)
	out := auditFlag.GetString("output")
	if out != "stdout" {
		out = auditFlag.GetPath("output")
	}
	x.AssertTruef(out != "", "out flag is not provided for the audit logs")
	encBytes, err := readAuditEncKey(auditFlag)
	x.Check(err)
	return &x.LoggerConf{
		Compress:      auditFlag.GetBool("compress"),
		Output:        out,
		EncryptionKey: encBytes,
		Days:          auditFlag.GetInt64("days"),
		Size:          auditFlag.GetInt64("size"),
		MessageKey:    "endpoint",
	}
}

func readAuditEncKey(conf *z.SuperFlag) ([]byte, error) {
	encFile := conf.GetPath("encrypt-file")
	if encFile == "" {
		return nil, nil
	}
	encKey, err := os.ReadFile(encFile)
	if err != nil {
		return nil, err
	}
	return encKey, nil
}

// InitAuditorIfNecessary accepts conf and initialized auditor.
func InitAuditorIfNecessary(conf *x.LoggerConf) error {
	if conf == nil {
		return nil
	}
	return InitAuditor(conf, uint64(worker.GroupId()), worker.NodeId())
}

// InitAuditor initializes the auditor.
func InitAuditor(conf *x.LoggerConf, gId, nId uint64) error {
	ntype := NodeTypeAlpha
	if gId == 0 {
		ntype = NodeTypeZero
	}
	var err error
	filename := fmt.Sprintf(defaultAuditFilenameF, ntype, gId, nId)
	if auditor.log, err = x.InitLogger(conf, filename); err != nil {
		return err
	}
	auditEnabled.Store(1)
	glog.Infoln("audit logs are enabled")
	return nil
}

// Close stops the ticker and sync the pending logs in buffer.
// It also sets the log to nil, because its being called by zero when license expires.
// If license added, InitLogger will take care of the file.
func Close() {
	if auditEnabled.Load() == 0 {
		return
	}
	auditor.log.Sync()
	auditor.log = nil
	glog.Infoln("audit logs are closed.")
}

func (a *auditLogger) Audit(event *AuditEvent) {
	a.log.AuditI(event.Endpoint,
		"level", "AUDIT",
		"user", event.User,
		"namespace", event.Namespace,
		"server", event.ServerHost,
		"client", event.ClientHost,
		"req_type", event.ReqType,
		"req_body", event.Req,
		"query_param", event.QueryParams,
		"status", event.Status)
}
