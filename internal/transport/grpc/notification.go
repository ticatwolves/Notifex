package grpc

import (
	"context"
	pb "notifex/proto/notification"

	"notifex/internal/domain/notification"
)

type NotificationServer struct {
	pb.UnimplementedNotificationServiceServer

	service *notification.Service
}

func NewNotificationServer(service *notification.Service) *NotificationServer {
	return &NotificationServer{
		service: service,
	}
}

func (s *NotificationServer) SendNotification(
	ctx context.Context,
	req *pb.SendNotificationRequest,
) (*pb.SendNotificationResponse, error) {

	resp, err := s.service.Send(ctx, notification.SendRequest{
		NotificationID: req.NotificationId,
		Payload:        req.Payload,
		Context:        req.Context,
	})

	if err != nil {
		return nil, err
	}

	return &pb.SendNotificationResponse{
		RequestId: resp.RequestID,
	}, nil
}

func (s *NotificationServer) SendBulkNotification(
	ctx context.Context,
	req *pb.SendBulkNotificationRequest,
) (*pb.SendBulkNotificationResponse, error) {
	notificationIds := make([]string, 0, len(req.Notifications))
	for _, notificationReq := range req.Notifications {
		notification, err := s.service.Send(ctx, notification.SendRequest{
			NotificationID: notificationReq.NotificationId,
			Payload:        notificationReq.Payload,
			Context:        notificationReq.Context,
		})
		if err != nil {
			return nil, err
		}
		notificationIds = append(notificationIds, notification.RequestID)
	}
	return &pb.SendBulkNotificationResponse{
		RequestIds: notificationIds,
	}, nil
}
