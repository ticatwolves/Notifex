package main

import (
	"log"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"notifex/internal/bootstrap"
	grpcTransport "notifex/internal/transport/grpc"

	"notifex/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	container, err := bootstrap.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	lis, err := net.Listen("tcp", cfg.GRPCAddressPort)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			grpcTransport.APIKeyInterceptor(container.APIKeyService),
		),
	)
	grpcTransport.RegisterServices(server, container)
	reflection.Register(server)
	log.Printf("gRPC server listening on %s", cfg.GRPCAddressPort)
	if err := server.Serve(lis); err != nil {
		log.Fatal(err)
	}
}
