// Package sqstest starts one ElasticMQ (SQS-compatible) container per test
// binary, or uses NOTIFEX_TEST_SQS_ENDPOINT. Tests create queues under a
// unique prefix so they don't interfere.
package sqstest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the ElasticMQ image (keep in sync with docker-compose.yml).
const Image = "softwaremill/elasticmq-native@sha256:e4580ab9ad1bd5cd37b4ba04911bc5ccc8cd2d9ab4de56ece65acee71c24e05c"

var (
	once     sync.Once
	endpoint string
	initErr  error
)

// Client returns an SQS client for the shared ElasticMQ.
func Client(t testing.TB) *sqs.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs Docker/ElasticMQ (run without -short)")
	}
	once.Do(func() { endpoint, initErr = start() })
	if initErr != nil {
		t.Fatalf("sqstest: %v", initErr)
	}
	return sqs.New(sqs.Options{
		Region: "ap-south-1", BaseEndpoint: aws.String(endpoint),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
}

// Prefix returns a unique queue name prefix for one test.
func Prefix() string {
	return "t" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "-dispatch-"
}

func start() (string, error) {
	if e := os.Getenv("NOTIFEX_TEST_SQS_ENDPOINT"); e != "" {
		return e, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: Image, ExposedPorts: []string{"9324/tcp"},
			WaitingFor: wait.ForListeningPort("9324/tcp").WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return "", fmt.Errorf("start elasticmq: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("elasticmq host: %w", err)
	}
	port, err := c.MappedPort(ctx, "9324/tcp")
	if err != nil {
		return "", fmt.Errorf("elasticmq port: %w", err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port()), nil
}
