/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hypermodeinc/dgraph/v24/x"
)

const (
	// AllowMutations is the mode allowing all mutations.
	AllowMutations int = iota
	// DisallowMutations is the mode that disallows all mutations.
	DisallowMutations
	// StrictMutations is the mode that allows mutations if and only if they contain known preds.
	StrictMutations
)

// Options contains options for the Dgraph server.
type Options struct {
	// PostingDir is the path to the directory storing the postings..
	PostingDir string
	// WALDir is the path to the directory storing the write-ahead log.
	WALDir string
	// MutationsMode is the mode used to handle mutation requests.
	MutationsMode int
	// AuthToken is the token to be passed for Alter HTTP requests.
	AuthToken string

	// AclJwtAlg stores the JWT signing algorithm.
	AclJwtAlg jwt.SigningMethod
	// AclSecretKey stores the secret used to sign JSON Web Tokens (JWT).
	// It could be a either a RSA or ECDSA PrivateKey or HMAC symmetric key.
	// depending upon the JWT signing algorithm. Public key can be derived
	// from the private key to verify the signatures when needed.
	AclSecretKey      interface{}
	AclSecretKeyBytes x.Sensitive
	// AccessJwtTtl is the TTL for the access JWT.
	AccessJwtTtl time.Duration
	// RefreshJwtTtl is the TTL of the refresh JWT.
	RefreshJwtTtl time.Duration

	// CachePercentage is the comma-separated list of cache percentages
	// used to split the total cache size among the multiple caches.
	CachePercentage string
	// CacheMb is the total memory allocated between all the caches.
	CacheMb int64
	// RemoveOnUpdate is the parameter that allows the user to set if the cache should keep the items that were
	// just mutated. Keeping these items are good when there is a mixed workload where you are updating the
	// same element multiple times. However, for a heavy mutation workload, not keeping these items would be better
	// , as keeping these elements bloats the cache making it slow.
	RemoveOnUpdate bool

	Audit *x.LoggerConf

	// Define different ChangeDataCapture configurations
	ChangeDataConf string

	// TypeFilterUidLimit decides how many elements would be searched directly
	// vs searched via type index. If the number of elements are too low, then querying the
	// index might be slower. This would allow people to set their limit according to
	// their use case.
	TypeFilterUidLimit uint64
}

// Config holds an instance of the server options..
var Config Options

// SetConfiguration sets the server configuration to the given config.
func SetConfiguration(newConfig *Options) {
	if newConfig == nil {
		return
	}
	newConfig.validate()
	Config = *newConfig
}

// AvailableMemory is the total size of the memory we were able to identify.
var AvailableMemory int64

func (opt *Options) validate() {
	pd, err := filepath.Abs(opt.PostingDir)
	x.Check(err)
	wd, err := filepath.Abs(opt.WALDir)
	x.Check(err)
	td, err := filepath.Abs(x.WorkerConfig.TmpDir)
	x.Check(err)
	x.AssertTruef(pd != wd,
		"Posting and WAL directory cannot be the same ('%s').", opt.PostingDir)
	x.AssertTruef(pd != td,
		"Posting and Tmp directory cannot be the same ('%s').", opt.PostingDir)
	x.AssertTruef(wd != td,
		"WAL and Tmp directory cannot be the same ('%s').", opt.WALDir)
	if opt.Audit != nil {
		ad, err := filepath.Abs(opt.Audit.Output)
		x.Check(err)
		x.AssertTruef(ad != pd,
			"Posting directory and Audit Output cannot be the same ('%s').", opt.Audit.Output)
		x.AssertTruef(ad != wd,
			"WAL directory and Audit Output cannot be the same ('%s').", opt.Audit.Output)
		x.AssertTruef(ad != td,
			"Tmp directory and Audit Output cannot be the same ('%s').", opt.Audit.Output)
	}
}
