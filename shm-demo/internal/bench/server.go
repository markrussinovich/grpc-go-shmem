// Copyright 2026 gRPC SHM Demo authors.
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

package bench

import (
	"context"
	"io"

	pb "shmdemo/proto/shmdemobench"
)

// server implements the generated BenchmarkService. This handler is identical
// regardless of transport.
type server struct {
	pb.UnimplementedBenchmarkServiceServer
}

// NewServer returns a BenchmarkService implementation.
func NewServer() pb.BenchmarkServiceServer { return &server{} }

// UnaryCall echoes a response of the requested size.
func (s *server) UnaryCall(_ context.Context, req *pb.SimpleRequest) (*pb.SimpleResponse, error) {
	return &pb.SimpleResponse{Payload: &pb.Payload{Body: make([]byte, req.GetResponseSize())}}, nil
}

// StreamingCall echoes each request as a response of the requested size over a
// single bidi stream. Both the latency and throughput phases drive it the same
// way — bounded-in-flight ping-pong with response_size > 0 — so the server runs
// identical work for either measurement.
func (s *server) StreamingCall(stream pb.BenchmarkService_StreamingCallServer) error {
	resp := &pb.SimpleResponse{Payload: &pb.Payload{}}
	var buf []byte
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		n := int(req.GetResponseSize())
		if n < 0 {
			n = 0
		}
		if cap(buf) < n {
			buf = make([]byte, n)
		}
		resp.Payload.Body = buf[:n]
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}
