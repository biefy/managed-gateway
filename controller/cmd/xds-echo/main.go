// xds-echo is a minimal Delta-ADS gRPC server. It logs every DeltaDiscoveryRequest
// received (node id, type_url, resources, metadata) and responds with an empty
// DeltaDiscoveryResponse. Used during M4 bringup to capture agentgateway's
// on-the-wire node identity + subscription shape before we wire Istiod's dispatch.
package main

import (
	"io"
	"log"
	"net"
	"os"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

type server struct {
	discoveryv3.UnimplementedAggregatedDiscoveryServiceServer
}

var marshaler = protojson.MarshalOptions{Multiline: false, EmitUnpopulated: true}

func (s *server) DeltaAggregatedResources(stream discoveryv3.AggregatedDiscoveryService_DeltaAggregatedResourcesServer) error {
	log.Printf("delta stream opened")
	seen := 0
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			log.Printf("delta stream closed (EOF)")
			return nil
		}
		if err != nil {
			log.Printf("delta stream error: %v", err)
			return err
		}
		seen++
		if seen <= 6 {
			j, _ := marshaler.Marshal(req)
			log.Printf("DeltaDiscoveryRequest #%d: %s", seen, string(j))
		}
		// Do NOT send a response — we want to capture the clean initial shape,
		// not get into an ACK ping-pong with nonces.
	}
}

func (s *server) StreamAggregatedResources(stream discoveryv3.AggregatedDiscoveryService_StreamAggregatedResourcesServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		j, _ := marshaler.Marshal(req)
		log.Printf("SotW DiscoveryRequest: %s", string(j))
		if err := stream.Send(&discoveryv3.DiscoveryResponse{TypeUrl: req.GetTypeUrl(), VersionInfo: "v0", Nonce: "n0"}); err != nil {
			return err
		}
	}
}

func main() {
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":15010"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	s := grpc.NewServer()
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(s, &server{})
	log.Printf("xds-echo listening on %s", addr)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
