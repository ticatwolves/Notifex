package grpc

import (
	gogrpc "google.golang.org/grpc"

	"notifex/internal/bootstrap"

	pb "notifex/proto/notification"
)

func RegisterServices(server *gogrpc.Server, app *bootstrap.App) {
	pb.RegisterNotificationServiceServer(
		server,
		NewNotificationServer(app.NotificationService),
	)
}
