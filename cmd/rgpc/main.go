package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"

	pb "notifex/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var (
	port = flag.Int("port", 9090, "gRPC server port")
)

type server struct {
	pb.UnimplementedNotificationServiceServer
}

func (s *server) CreateNotification(
	ctx context.Context,
	in *pb.NotificationRequest,
) (*pb.NotificationResponse, error) {

	log.Printf("NotificationPayload: %s", in.GetId())

	return &pb.NotificationResponse{
		Id: in.GetId(),
	}, nil
}

func main() {
	flag.Parse()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	reflection.Register(grpcServer) // Add this line

	pb.RegisterNotificationServiceServer(
		grpcServer,
		&server{},
	)

	log.Printf("server listening at %v", lis.Addr())

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
