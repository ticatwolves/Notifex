package grpc

import (
	"context"

	"notifex/internal/domain/apikey"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func APIKeyInterceptor(apiKeyService *apikey.Service) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Extract API key from metadata
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Errorf(codes.Unauthenticated, "missing metadata")
		}

		apiKeys := md.Get("x-api-key")
		if len(apiKeys) == 0 {
			return nil, status.Errorf(codes.Unauthenticated, "missing API key")
		}

		apiKey := apiKeys[0]

		// Validate API key
		valid, err := apiKeyService.ValidateAPIKey(ctx, apiKey)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to validate API key: %v", err)
		}
		if !valid {
			return nil, status.Errorf(codes.Unauthenticated, "invalid API key")
		}

		// Continue handling the request
		return handler(ctx, req)
	}
}
