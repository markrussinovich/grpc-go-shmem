/*
 *
 * Copyright 2025 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package shmem implements a load balancing policy that prefers shared memory
// transport for local endpoints. This is part of RFC A73 Phase 6.
//
// The policy inspects endpoint attributes to identify shmem-capable endpoints
// and prioritizes them over TCP endpoints when both the client and server are
// on the same machine.
//
// Usage:
//
//	import _ "google.golang.org/grpc/balancer/shmem"
//
//	// Set service config to use shmem_prefer policy
//	conn, err := grpc.Dial(target,
//	    grpc.WithDefaultServiceConfig(`{"loadBalancingConfig": [{"shmem_prefer":{}}]}`))
package shmem

import (
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	internalgrpclog "google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

func init() {
	balancer.Register(builder{})
}

// Name is the name of the shmem-prefer load balancer.
const Name = "shmem_prefer"

var logger = grpclog.Component("shmem-lb")

// TransportPreference indicates the preferred transport for an endpoint.
type TransportPreference int

const (
	// TransportPreferenceDefault indicates no preference (use any transport).
	TransportPreferenceDefault TransportPreference = iota
	// TransportPreferenceShmem indicates preference for shared memory transport.
	TransportPreferenceShmem
	// TransportPreferenceTCP indicates preference for TCP transport.
	TransportPreferenceTCP
)

// transportPreferenceKey is the attribute key for transport preference.
type transportPreferenceKey struct{}

// SetTransportPreference sets the transport preference on an address.
func SetTransportPreference(addr resolver.Address, pref TransportPreference) resolver.Address {
	addr.BalancerAttributes = addr.BalancerAttributes.WithValue(transportPreferenceKey{}, pref)
	return addr
}

// GetTransportPreference retrieves the transport preference from an address.
func GetTransportPreference(addr resolver.Address) TransportPreference {
	v := addr.BalancerAttributes.Value(transportPreferenceKey{})
	if v == nil {
		return TransportPreferenceDefault
	}
	pref, ok := v.(TransportPreference)
	if !ok {
		return TransportPreferenceDefault
	}
	return pref
}

// SetEndpointTransportPreference sets the transport preference on an endpoint.
func SetEndpointTransportPreference(ep resolver.Endpoint, pref TransportPreference) resolver.Endpoint {
	ep.Attributes = ep.Attributes.WithValue(transportPreferenceKey{}, pref)
	return ep
}

// GetEndpointTransportPreference retrieves the transport preference from an endpoint.
func GetEndpointTransportPreference(ep resolver.Endpoint) TransportPreference {
	if ep.Attributes == nil {
		return TransportPreferenceDefault
	}
	v := ep.Attributes.Value(transportPreferenceKey{})
	if v == nil {
		return TransportPreferenceDefault
	}
	pref, ok := v.(TransportPreference)
	if !ok {
		return TransportPreferenceDefault
	}
	return pref
}

// isLocalKeyType is the attribute key for marking an endpoint as local.
type isLocalKeyType struct{}

// SetLocalEndpoint marks an endpoint as being on the local machine.
func SetLocalEndpoint(ep resolver.Endpoint, isLocal bool) resolver.Endpoint {
	ep.Attributes = ep.Attributes.WithValue(isLocalKeyType{}, isLocal)
	return ep
}

// IsLocalEndpoint checks if an endpoint is marked as local.
func IsLocalEndpoint(ep resolver.Endpoint) bool {
	if ep.Attributes == nil {
		return false
	}
	v := ep.Attributes.Value(isLocalKeyType{})
	if v == nil {
		return false
	}
	isLocal, ok := v.(bool)
	return ok && isLocal
}

// SetLocalAddress marks an address as being on the local machine.
func SetLocalAddress(addr resolver.Address, isLocal bool) resolver.Address {
	addr.BalancerAttributes = addr.BalancerAttributes.WithValue(isLocalKeyType{}, isLocal)
	return addr
}

// IsLocalAddress checks if an address is marked as local.
func IsLocalAddress(addr resolver.Address) bool {
	v := addr.BalancerAttributes.Value(isLocalKeyType{})
	if v == nil {
		return false
	}
	isLocal, ok := v.(bool)
	return ok && isLocal
}

type builder struct{}

func (builder) Name() string {
	return Name
}

func (builder) Build(cc balancer.ClientConn, _ balancer.BuildOptions) balancer.Balancer {
	b := &shmemPreferBalancer{
		cc:       cc,
		subConns: resolver.NewAddressMapV2[balancer.SubConn](),
		scStates: make(map[balancer.SubConn]connectivity.State),
		csEvltr:  &balancer.ConnectivityStateEvaluator{},
		state:    connectivity.Connecting,
	}
	b.logger = internalgrpclog.NewPrefixLogger(logger, fmt.Sprintf("[shmem-prefer-lb %p] ", b))
	return b
}

func (builder) ParseConfig(js json.RawMessage) (serviceconfig.LoadBalancingConfig, error) {
	var cfg shmemPreferConfig
	if err := json.Unmarshal(js, &cfg); err != nil {
		return nil, fmt.Errorf("shmem_prefer: error parsing config: %v", err)
	}
	return &cfg, nil
}

type shmemPreferConfig struct {
	serviceconfig.LoadBalancingConfig `json:"-"`
}

// shmemPreferBalancer implements a load balancer that prefers shmem connections.
type shmemPreferBalancer struct {
	logger *internalgrpclog.PrefixLogger
	cc     balancer.ClientConn

	mu       sync.Mutex
	subConns *resolver.AddressMapV2[balancer.SubConn]
	scStates map[balancer.SubConn]connectivity.State
	csEvltr  *balancer.ConnectivityStateEvaluator
	state    connectivity.State

	// Addresses sorted by preference (shmem first, then TCP)
	sortedAddrs []resolver.Address
}

func (b *shmemPreferBalancer) UpdateClientConnState(s balancer.ClientConnState) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.logger.V(2) {
		b.logger.Infof("Received update: %s", pretty(s))
	}

	// Collect all addresses from endpoints
	var allAddrs []resolver.Address
	if len(s.ResolverState.Endpoints) > 0 {
		for _, ep := range s.ResolverState.Endpoints {
			for _, addr := range ep.Addresses {
				// Copy endpoint attributes to address balancer attributes
				pref := GetEndpointTransportPreference(ep)
				if pref != TransportPreferenceDefault {
					addr = SetTransportPreference(addr, pref)
				}
				if IsLocalEndpoint(ep) {
					addr = SetLocalAddress(addr, true)
				}
				allAddrs = append(allAddrs, addr)
			}
		}
	} else {
		allAddrs = s.ResolverState.Addresses
	}

	if len(allAddrs) == 0 {
		b.ResolverError(fmt.Errorf("produced zero addresses"))
		return balancer.ErrBadResolverState
	}

	// Sort addresses: shmem-capable local first, then others
	b.sortedAddrs = sortAddressesByPreference(allAddrs)

	// Create set of new addresses
	addrsSet := resolver.NewAddressMapV2[any]()
	for _, addr := range b.sortedAddrs {
		addrsSet.Set(addr, nil)
	}

	// Remove SubConns that are no longer in the address list
	for _, addr := range b.subConns.Keys() {
		if _, ok := addrsSet.Get(addr); !ok {
			sc, _ := b.subConns.Get(addr)
			sc.Shutdown()
			b.subConns.Delete(addr)
		}
	}

	// Add new SubConns for new addresses
	for _, addr := range b.sortedAddrs {
		if _, ok := b.subConns.Get(addr); !ok {
			sc, err := b.cc.NewSubConn([]resolver.Address{addr}, balancer.NewSubConnOptions{
				StateListener: func(state balancer.SubConnState) {
					b.updateSubConnState(addr, state)
				},
			})
			if err != nil {
				b.logger.Warningf("Failed to create SubConn for %v: %v", addr, err)
				continue
			}
			b.subConns.Set(addr, sc)
			b.scStates[sc] = connectivity.Idle
			sc.Connect()
		}
	}

	// If we have at least one SubConn, we're in a good state
	if b.subConns.Len() == 0 {
		b.state = connectivity.TransientFailure
		b.cc.UpdateState(balancer.State{
			ConnectivityState: connectivity.TransientFailure,
			Picker:            base.NewErrPicker(balancer.ErrNoSubConnAvailable),
		})
	}

	return nil
}

func (b *shmemPreferBalancer) updateSubConnState(addr resolver.Address, state balancer.SubConnState) {
	b.mu.Lock()
	defer b.mu.Unlock()

	sc, ok := b.subConns.Get(addr)
	if !ok {
		return
	}
	oldState := b.scStates[sc]
	b.scStates[sc] = state.ConnectivityState

	if b.logger.V(2) {
		b.logger.Infof("SubConn state change: %v -> %v for %v", oldState, state.ConnectivityState, addr)
	}

	b.state = b.csEvltr.RecordTransition(oldState, state.ConnectivityState)

	// Build picker with current ready subconns
	b.regeneratePickerLocked()
}

func (b *shmemPreferBalancer) regeneratePickerLocked() {
	// Collect ready SubConns in preference order
	var readySCs []balancer.SubConn
	var readyAddrs []resolver.Address
	var shmemReadySCs []balancer.SubConn
	var tcpReadySCs []balancer.SubConn

	for _, addr := range b.sortedAddrs {
		sc, ok := b.subConns.Get(addr)
		if !ok {
			continue
		}
		if b.scStates[sc] == connectivity.Ready {
			readySCs = append(readySCs, sc)
			readyAddrs = append(readyAddrs, addr)
			// Classify by transport preference
			pref := GetTransportPreference(addr)
			isLocal := IsLocalAddress(addr)
			if pref == TransportPreferenceShmem || isLocal {
				shmemReadySCs = append(shmemReadySCs, sc)
			} else {
				tcpReadySCs = append(tcpReadySCs, sc)
			}
		}
	}

	if len(readySCs) == 0 {
		// No ready subconns, report current state
		b.cc.UpdateState(balancer.State{
			ConnectivityState: b.state,
			Picker:            base.NewErrPicker(balancer.ErrNoSubConnAvailable),
		})
		return
	}

	// Create picker that prefers shmem connections
	picker := &shmemPreferPicker{
		shmemSCs: shmemReadySCs,
		tcpSCs:   tcpReadySCs,
		allSCs:   readySCs,
		allAddrs: readyAddrs,
	}

	b.cc.UpdateState(balancer.State{
		ConnectivityState: connectivity.Ready,
		Picker:            picker,
	})
}

func (b *shmemPreferBalancer) ResolverError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.logger.V(2) {
		b.logger.Infof("Resolver error: %v", err)
	}

	if b.subConns.Len() == 0 {
		b.state = connectivity.TransientFailure
		b.cc.UpdateState(balancer.State{
			ConnectivityState: connectivity.TransientFailure,
			Picker:            base.NewErrPicker(fmt.Errorf("resolver error: %v", err)),
		})
	}
}

func (b *shmemPreferBalancer) UpdateSubConnState(sc balancer.SubConn, state balancer.SubConnState) {
	b.logger.Errorf("UpdateSubConnState(%v, %+v) called unexpectedly", sc, state)
}

func (b *shmemPreferBalancer) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, addr := range b.subConns.Keys() {
		sc, _ := b.subConns.Get(addr)
		sc.Shutdown()
	}
	b.subConns = resolver.NewAddressMapV2[balancer.SubConn]()
	b.scStates = make(map[balancer.SubConn]connectivity.State)
}

// ExitIdle is called when the ClientConn wants the balancer to exit idle.
func (b *shmemPreferBalancer) ExitIdle() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Trigger connection on all idle SubConns
	for _, addr := range b.subConns.Keys() {
		sc, ok := b.subConns.Get(addr)
		if !ok {
			continue
		}
		if b.scStates[sc] == connectivity.Idle {
			sc.Connect()
		}
	}
}

// shmemPreferPicker picks SubConns with preference for shmem.
type shmemPreferPicker struct {
	shmemSCs []balancer.SubConn
	tcpSCs   []balancer.SubConn
	allSCs   []balancer.SubConn
	allAddrs []resolver.Address

	mu    sync.Mutex
	next  int
	sNext int
}

func (p *shmemPreferPicker) Pick(_ balancer.PickInfo) (balancer.PickResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Prefer shmem connections if available
	if len(p.shmemSCs) > 0 {
		sc := p.shmemSCs[p.sNext%len(p.shmemSCs)]
		p.sNext++
		return balancer.PickResult{SubConn: sc}, nil
	}

	// Fall back to any available connection
	if len(p.allSCs) > 0 {
		sc := p.allSCs[p.next%len(p.allSCs)]
		p.next++
		return balancer.PickResult{SubConn: sc}, nil
	}

	return balancer.PickResult{}, balancer.ErrNoSubConnAvailable
}

// sortAddressesByPreference sorts addresses with shmem-capable local addresses first.
func sortAddressesByPreference(addrs []resolver.Address) []resolver.Address {
	var shmemAddrs, tcpAddrs []resolver.Address

	for _, addr := range addrs {
		pref := GetTransportPreference(addr)
		isLocal := IsLocalAddress(addr)

		if pref == TransportPreferenceShmem || isLocal {
			shmemAddrs = append(shmemAddrs, addr)
		} else {
			tcpAddrs = append(tcpAddrs, addr)
		}
	}

	// Return shmem addresses first, then TCP
	result := make([]resolver.Address, 0, len(addrs))
	result = append(result, shmemAddrs...)
	result = append(result, tcpAddrs...)
	return result
}

func pretty(s any) string {
	b, _ := json.MarshalIndent(s, "", "  ")
	return string(b)
}
