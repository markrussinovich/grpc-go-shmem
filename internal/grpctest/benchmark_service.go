// Copyright 2024 gRPC authors.
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

// Package grpctest provides testing utilities for gRPC, including
// benchmark service definitions.
package grpctest

import (
	"context"
)

// SimpleRequest for benchmark testing
type SimpleRequest struct {
	Payload []byte
}

// SimpleResponse for benchmark testing
type SimpleResponse struct {
	Payload []byte
}

// EchoTestService is a simple service for benchmarking
type EchoTestService interface {
	UnaryCall(context.Context, *SimpleRequest) (*SimpleResponse, error)
	StreamingCall(EchoTestServiceStreamingCallServer) error
}

// EchoTestServiceStreamingCallServer is the server-side stream interface
type EchoTestServiceStreamingCallServer interface {
	Send(*SimpleResponse) error
	Recv() (*SimpleRequest, error)
}

// EchoTestServiceStreamingCallClient is the client-side stream interface
type EchoTestServiceStreamingCallClient interface {
	Send(*SimpleRequest) error
	Recv() (*SimpleResponse, error)
	CloseSend() error
}

// UnimplementedEchoTestServiceServer can be embedded for forward compatibility
type UnimplementedEchoTestServiceServer struct{}

// UnaryCall is a stub implementation that returns nil.
func (UnimplementedEchoTestServiceServer) UnaryCall(context.Context, *SimpleRequest) (*SimpleResponse, error) {
	return nil, nil
}

// StreamingCall is a stub implementation that returns nil.
func (UnimplementedEchoTestServiceServer) StreamingCall(EchoTestServiceStreamingCallServer) error {
	return nil
}

// RegisterEchoTestServiceServer registers the service (stub for now)
func RegisterEchoTestServiceServer(_ any, _ EchoTestService) {
	// This is a stub - actual implementation would register with gRPC server
}

// NewEchoTestServiceClient creates a new client (stub for now)
func NewEchoTestServiceClient(_ any) EchoTestServiceClient {
	return &echoTestServiceClient{}
}

// EchoTestServiceClient is the client API
type EchoTestServiceClient interface {
	UnaryCall(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error)
	StreamingCall(ctx context.Context) (EchoTestServiceStreamingCallClient, error)
}

type echoTestServiceClient struct{}

func (c *echoTestServiceClient) UnaryCall(_ context.Context, _ *SimpleRequest) (*SimpleResponse, error) {
	return &SimpleResponse{}, nil
}

func (c *echoTestServiceClient) StreamingCall(_ context.Context) (EchoTestServiceStreamingCallClient, error) {
	return nil, nil
}
