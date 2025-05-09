// Copyright 2016 The etcd Authors
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

package grpcproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/time/rate"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/naming/endpoints"
)

// allow maximum 1 retry per second
const resolveRetryRate = 1

type clusterProxy struct {
	lg   *zap.Logger
	clus pb.ClusterClient
	ctx  context.Context

	// advertise client URL
	advaddr string
	prefix  string

	em endpoints.Manager

	umu  sync.RWMutex
	umap map[string]endpoints.Endpoint
}

// NewClusterProxy takes optional prefix to fetch grpc-proxy member endpoints.
// The returned channel is closed when there is grpc-proxy endpoint registered
// and the client's context is canceled so the 'register' loop returns.
// TODO: Expand the API to report creation errors
func NewClusterProxy(lg *zap.Logger, c *clientv3.Client, advaddr string, prefix string) (pb.ClusterServer, <-chan struct{}) {
	if lg == nil {
		lg = zap.NewNop()
	}

	var em endpoints.Manager
	if advaddr != "" && prefix != "" {
		var err error
		if em, err = endpoints.NewManager(c, prefix); err != nil {
			lg.Error("failed to provision endpointsManager", zap.String("prefix", prefix), zap.Error(err))
			return nil, nil
		}
	}

	cp := &clusterProxy{
		lg:   lg,
		clus: pb.NewClusterClient(c.ActiveConnection()),
		ctx:  c.Ctx(),

		advaddr: advaddr,
		prefix:  prefix,
		umap:    make(map[string]endpoints.Endpoint),
		em:      em,
	}

	donec := make(chan struct{})
	if em != nil {
		go func() {
			defer close(donec)
			cp.establishEndpointWatch(prefix)
		}()
		return cp, donec
	}

	close(donec)
	return cp, donec
}

func (cp *clusterProxy) establishEndpointWatch(prefix string) {
	rm := rate.NewLimiter(rate.Limit(resolveRetryRate), resolveRetryRate)
	for rm.Wait(cp.ctx) == nil {
		wc, err := cp.em.NewWatchChannel(cp.ctx)
		if err != nil {
			cp.lg.Warn("failed to establish endpoint watch", zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		cp.monitor(wc)
	}
}

func (cp *clusterProxy) monitor(wa endpoints.WatchChannel) {
	for {
		select {
		case <-cp.ctx.Done():
			cp.lg.Info("watching endpoints interrupted", zap.Error(cp.ctx.Err()))
			return
		case updates, ok := <-wa:
			if !ok {
				cp.lg.Info("endpoints watch channel closed")
				return
			}
			cp.umu.Lock()
			for _, up := range updates {
				switch up.Op {
				case endpoints.Add:
					cp.umap[up.Key] = up.Endpoint
				case endpoints.Delete:
					delete(cp.umap, up.Key)
				}
			}
			cp.umu.Unlock()
		}
	}
}

func (cp *clusterProxy) MemberAdd(ctx context.Context, r *pb.MemberAddRequest) (*pb.MemberAddResponse, error) {
	return cp.clus.MemberAdd(ctx, r)
}

func (cp *clusterProxy) MemberRemove(ctx context.Context, r *pb.MemberRemoveRequest) (*pb.MemberRemoveResponse, error) {
	return cp.clus.MemberRemove(ctx, r)
}

func (cp *clusterProxy) MemberUpdate(ctx context.Context, r *pb.MemberUpdateRequest) (*pb.MemberUpdateResponse, error) {
	return cp.clus.MemberUpdate(ctx, r)
}

func (cp *clusterProxy) membersFromUpdates() ([]*pb.Member, error) {
	cp.umu.RLock()
	defer cp.umu.RUnlock()
	mbs := make([]*pb.Member, 0, len(cp.umap))
	for _, upt := range cp.umap {
		m, err := decodeMeta(fmt.Sprint(upt.Metadata))
		if err != nil {
			return nil, err
		}
		mbs = append(mbs, &pb.Member{Name: m.Name, ClientURLs: []string{upt.Addr}})
	}
	return mbs, nil
}

// MemberList wraps member list API with following rules:
// - If 'advaddr' is not empty and 'prefix' is not empty, return registered member lists via resolver
// - If 'advaddr' is not empty and 'prefix' is not empty and registered grpc-proxy members haven't been fetched, return the 'advaddr'
// - If 'advaddr' is not empty and 'prefix' is empty, return 'advaddr' without forcing it to 'register'
// - If 'advaddr' is empty, forward to member list API
func (cp *clusterProxy) MemberList(ctx context.Context, r *pb.MemberListRequest) (*pb.MemberListResponse, error) {
	if cp.advaddr != "" {
		if cp.prefix != "" {
			mbs, err := cp.membersFromUpdates()
			if err != nil {
				return nil, err
			}
			if len(mbs) > 0 {
				return &pb.MemberListResponse{Members: mbs}, nil
			}
		}
		// prefix is empty or no grpc-proxy members haven't been registered
		hostname, _ := os.Hostname()
		return &pb.MemberListResponse{Members: []*pb.Member{{Name: hostname, ClientURLs: []string{cp.advaddr}}}}, nil
	}
	return cp.clus.MemberList(ctx, r)
}

func (cp *clusterProxy) MemberPromote(ctx context.Context, r *pb.MemberPromoteRequest) (*pb.MemberPromoteResponse, error) {
	// TODO: implement
	return nil, errors.New("not implemented")
}
