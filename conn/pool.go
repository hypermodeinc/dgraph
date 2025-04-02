/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package conn

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	// ErrNoConnection indicates no connection exists to a node.
	ErrNoConnection = errors.New("No connection exists")
	// ErrUnhealthyConnection indicates the connection to a node is unhealthy.
	ErrUnhealthyConnection = errors.New("Unhealthy connection")
	echoDuration           = 500 * time.Millisecond
)

// Pool is used to manage the grpc client connection(s) for communicating with other
// worker instances.  Right now it just holds one of them.
type Pool struct {
	sync.RWMutex
	// A pool now consists of one connection. gRPC uses HTTP2 transport to combine
	// messages in the same TCP stream.
	conn *grpc.ClientConn

	lastEcho   time.Time
	Addr       string
	closer     *z.Closer
	healthInfo pb.HealthInfo
	dialOpts   []grpc.DialOption
}

// Pools manages a concurrency-safe set of Pool.
type Pools struct {
	sync.RWMutex
	all map[string]*Pool
}

var pi *Pools

func init() {
	pi = new(Pools)
	pi.all = make(map[string]*Pool)
}

// GetPools returns the list of pools.
func GetPools() *Pools {
	return pi
}

// Get returns the list for the given address.
func (p *Pools) Get(addr string) (*Pool, error) {
	p.RLock()
	defer p.RUnlock()
	pool, ok := p.all[addr]
	if !ok {
		return nil, ErrNoConnection
	}
	if !pool.IsHealthy() {
		return nil, ErrUnhealthyConnection
	}
	return pool, nil
}

// GetAll returns all pool entries.
func (p *Pools) GetAll() []*Pool {
	p.RLock()
	defer p.RUnlock()
	var pool []*Pool
	for _, v := range p.all {
		pool = append(pool, v)
	}
	return pool
}

// RemoveInvalid removes invalid nodes from the list of pools.
func (p *Pools) RemoveInvalid(state *pb.MembershipState) {
	// Keeps track of valid IP addresses, assigned to active nodes. We do this
	// to avoid removing valid IP addresses from the Removed list.
	validAddr := make(map[string]struct{})
	for _, group := range state.Groups {
		for _, member := range group.Members {
			validAddr[member.Addr] = struct{}{}
		}
	}
	for _, member := range state.Zeros {
		validAddr[member.Addr] = struct{}{}
	}
	for _, member := range state.Removed {
		// Some nodes could have the same IP address. So, check before disconnecting.
		if _, valid := validAddr[member.Addr]; !valid {
			p.remove(member.Addr)
		}
	}
}

func (p *Pools) remove(addr string) {
	p.Lock()
	defer p.Unlock()
	pool, ok := p.all[addr]
	if !ok {
		return
	}
	glog.Warningf("CONN: Disconnecting from %s\n", addr)
	delete(p.all, addr)
	pool.shutdown()
}

func (p *Pools) getPool(addr string) (*Pool, bool) {
	p.RLock()
	defer p.RUnlock()
	existingPool, has := p.all[addr]
	return existingPool, has
}

// Connect creates a Pool instance for the node with the given address or returns the existing one.
func (p *Pools) Connect(addr string, tlsClientConf *tls.Config) *Pool {
	existingPool, has := p.getPool(addr)
	if has {
		return existingPool
	}

	pool, err := newPool(addr, tlsClientConf)
	if err != nil {
		glog.Errorf("CONN: Unable to connect to host: %s", addr)
		return nil
	}

	p.Lock()
	defer p.Unlock()
	existingPool, has = p.all[addr]
	if has {
		go pool.shutdown() // Not being used, so release the resources.
		return existingPool
	}
	glog.Infof("CONN: Connecting to %s\n", addr)
	p.all[addr] = pool
	return pool
}

// newPool creates a new "pool" with one gRPC connection, refcount 0.
func newPool(addr string, tlsClientConf *tls.Config) (*Pool, error) {
	conOpts := []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(x.GrpcMaxSize),
			grpc.MaxCallSendMsgSize(x.GrpcMaxSize),
			grpc.UseCompressor((snappyCompressor{}).Name())),
		grpc.WithBackoffMaxDelay(time.Second),
	}

	if tlsClientConf != nil {
		conOpts = append(conOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsClientConf)))
	} else {
		conOpts = append(conOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(addr, conOpts...)
	if err != nil {
		glog.Errorf("unable to connect with %s : %s", addr, err)
		return nil, err
	}

	pl := &Pool{
		conn:     conn,
		Addr:     addr,
		lastEcho: time.Now(),
		dialOpts: conOpts,
		closer:   z.NewCloser(1),
	}
	go pl.MonitorHealth()
	return pl, nil
}

// Get returns the connection to use from the pool of connections.
func (p *Pool) Get() *grpc.ClientConn {
	p.RLock()
	defer p.RUnlock()
	return p.conn
}

func (p *Pool) shutdown() {
	glog.Warningf("CONN: Shutting down extra connection to %s", p.Addr)
	p.closer.SignalAndWait()
	if err := p.conn.Close(); err != nil {
		glog.Warningf("Could not close pool connection with error: %s", err)
	}
}

// SetUnhealthy marks a pool as unhealthy.
func (p *Pool) SetUnhealthy() {
	p.Lock()
	defer p.Unlock()
	p.lastEcho = time.Time{}
}

func (p *Pool) listenToHeartbeat() error {
	conn := p.Get()
	c := pb.NewRaftClient(conn)

	ctx, cancel := context.WithCancel(p.closer.Ctx())
	defer cancel()

	s, err := c.Heartbeat(ctx, &api.Payload{})
	if err != nil {
		return err
	}

	go func() {
		for {
			res, err := s.Recv()
			if err != nil || res == nil {
				cancel()
				return
			}

			// We do this periodic stream receive based approach to defend against network partitions.
			p.Lock()
			p.lastEcho = time.Now()
			p.healthInfo = *res
			p.Unlock()
		}
	}()

	threshold := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Don't check before at least 10s since start.
			if time.Now().Before(threshold) {
				continue
			}
			p.RLock()
			lastEcho := p.lastEcho
			p.RUnlock()
			if dur := time.Since(lastEcho); dur > 30*time.Second {
				glog.Warningf("CONN: No echo to %s for %s. Cancelling connection heartbeats.\n",
					p.Addr, dur.Round(time.Second))
				cancel()
				return fmt.Errorf("too long since last echo")
			}

		case <-s.Context().Done():
			return s.Context().Err()
		case <-ctx.Done():
			return ctx.Err()
		case <-p.closer.HasBeenClosed():
			cancel()
			return p.closer.Ctx().Err()
		}
	}
}

// MonitorHealth monitors the health of the connection via Echo. This function blocks forever.
func (p *Pool) MonitorHealth() {
	defer p.closer.Done()

	// We might have lost connection to the destination. In that case, re-dial
	// the connection.
	reconnect := func() {
		for {
			time.Sleep(time.Second)
			if err := p.closer.Ctx().Err(); err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(p.closer.Ctx(), 10*time.Second)
			conn, err := grpc.NewClient(p.Addr, p.dialOpts...)
			if err == nil {
				// Make a dummy request to test out the connection.
				client := pb.NewRaftClient(conn)
				_, err = client.IsPeer(ctx, &pb.RaftContext{})
			}
			cancel()
			if err == nil {
				p.Lock()
				if err := p.conn.Close(); err != nil {
					glog.Warningf("error while closing connection: %v", err)
				}
				p.conn = conn
				p.Unlock()
				return
			}
			glog.Errorf("CONN: Unable to connect with %s : %s\n", p.Addr, err)
			if conn != nil {
				if err := conn.Close(); err != nil {
					glog.Warningf("error while closing connection: %v", err)
				}
			}
		}
	}

	for {
		select {
		case <-p.closer.HasBeenClosed():
			glog.Infof("CONN: Returning from MonitorHealth for %s", p.Addr)
			return
		default:
			err := p.listenToHeartbeat()
			if err != nil {
				reconnect()
				glog.Infof("CONN: Re-established connection with %s.\n", p.Addr)
			}
			// Sleep for a bit before retrying.
			time.Sleep(echoDuration)
		}
	}
}

// IsHealthy returns whether the pool is healthy.
func (p *Pool) IsHealthy() bool {
	if p == nil {
		return false
	}
	p.RLock()
	defer p.RUnlock()
	return time.Since(p.lastEcho) < 4*echoDuration
}

// HealthInfo returns the healthinfo.
func (p *Pool) HealthInfo() pb.HealthInfo {
	ok := p.IsHealthy()
	p.Lock()
	defer p.Unlock()
	p.healthInfo.Status = "healthy"
	if !ok {
		p.healthInfo.Status = "unhealthy"
	}
	p.healthInfo.LastEcho = p.lastEcho.Unix()
	return p.healthInfo
}
