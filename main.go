package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// Clients are initialized globally to be reused across lambda executions
var (
	s3Client        *s3.Client
	s3PresignClient *s3.PresignClient
	dynamodbClient  *dynamodb.Client
	tableName       = "ShareLoomMetadata"
)

func init() {
	// Initialize AWS config during the cold start Init phase
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatalf("unable to load SDK config, %v", err)
	}

	s3Client = s3.NewFromConfig(cfg)
	s3PresignClient = s3.NewPresignClient(s3Client)
	dynamodbClient = dynamodb.NewFromConfig(cfg)
}

// UploadResponseBody defines the successful JSON response for upload
type UploadResponseBody struct {
	UploadURL string `json:"uploadUrl"`
	FileID    string `json:"fileId"`
}

// DownloadResponseBody defines the successful JSON response for download
type DownloadResponseBody struct {
	DownloadURL string `json:"downloadUrl"`
}

// ErrorResponseBody defines the error JSON response structure
type ErrorResponseBody struct {
	Error string `json:"error"`
}

// MessageResponseBody defines a generic message response
type MessageResponseBody struct {
	Message string `json:"message"`
}

// corsHeaders returns the standard CORS headers for all responses
func corsHeaders() map[string]string {
	return map[string]string{
		"Access-Control-Allow-Origin": "*",
		"Content-Type":                "application/json",
	}
}

// Handler is the entry point for the Lambda function.
// Uses API Gateway HTTP API v2 event types.
func Handler(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	bucketName := os.Getenv("BUCKET_NAME")
	if bucketName == "" {
		log.Println("BUCKET_NAME environment variable is not set")
		return createErrorResponse("Internal server error configuration", 500), nil
	}

	// Extract HTTP method from v2 request context
	method := strings.ToUpper(req.RequestContext.HTTP.Method)

	// Extract fileId path parameter
	fileID := req.PathParameters["fileId"]

	switch method {
	case "POST":
		return handleUpload(ctx, bucketName)
	case "GET":
		return handleDownload(ctx, fileID, bucketName)
	case "DELETE":
		return handleDeleteFile(ctx, fileID, bucketName)
	case "OPTIONS":
		return handleOptions()
	default:
		return createErrorResponse("Method not allowed", 405), nil
	}
}

// handleUpload generates a presigned PUT URL and stores metadata in DynamoDB.
func handleUpload(ctx context.Context, bucketName string) (events.APIGatewayV2HTTPResponse, error) {
	now := time.Now()

	// Generate a unique fileId using UUID v4
	fileID := uuid.New().String()

	// Calculate expireAt for 24 hours in the future
	expireAt := now.Add(24 * time.Hour).Unix()

	// Insert a record into DynamoDB
	_, err := dynamodbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item: map[string]types.AttributeValue{
			"fileId":   &types.AttributeValueMemberS{Value: fileID},
			"expireAt": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expireAt)},
		},
	})
	if err != nil {
		log.Printf("Failed to put item in DynamoDB: %v", err)
		return createErrorResponse(fmt.Sprintf("Failed to save metadata: %v", err), 500), nil
	}

	// Generate an S3 Pre-signed URL of type PUT lasting 15 minutes
	presignReq, err := s3PresignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(fileID),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = 15 * time.Minute
	})
	if err != nil {
		log.Printf("Failed to generate pre-signed URL: %v", err)
		return createErrorResponse(fmt.Sprintf("Failed to generate upload URL: %v", err), 500), nil
	}

	// Respond with uploadUrl and fileId
	resBody := UploadResponseBody{
		UploadURL: presignReq.URL,
		FileID:    fileID,
	}

	bodyBytes, err := json.Marshal(resBody)
	if err != nil {
		log.Printf("Failed to marshal response body: %v", err)
		return createErrorResponse("Failed to encode response", 500), nil
	}

	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    corsHeaders(),
		Body:       string(bodyBytes),
	}, nil
}

// handleDownload implements the Burn After Reading pattern:
// 1. Validates fileId is present
// 2. Attempts a conditional DeleteItem to atomically verify and destroy the record
// 3. If the delete fails, returns 404
// 4. On success, generates a presigned GET URL with 2-minute expiration
func handleDownload(ctx context.Context, fileID string, bucketName string) (events.APIGatewayV2HTTPResponse, error) {
	if fileID == "" {
		return createErrorResponse("fileId is required", 400), nil
	}

	// Attempt conditional delete — atomically checks existence and removes the record
	_, err := dynamodbClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"fileId": &types.AttributeValueMemberS{Value: fileID},
		},
		ConditionExpression: aws.String("attribute_exists(fileId)"),
	})
	if err != nil {
		log.Printf("DeleteItem failed for fileId %s: %v", fileID, err)
		return createErrorResponse("El enlace ha caducado o ya fue utilizado", 404), nil
	}

	// Record successfully deleted — generate a presigned GET URL with 2-minute expiration
	presignReq, err := s3PresignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(fileID),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = 2 * time.Minute
	})
	if err != nil {
		log.Printf("Failed to generate presigned GET URL: %v", err)
		return createErrorResponse("Failed to generate download URL", 500), nil
	}

	// Return the download URL
	resBody := DownloadResponseBody{
		DownloadURL: presignReq.URL,
	}

	bodyBytes, err := json.Marshal(resBody)
	if err != nil {
		log.Printf("Failed to marshal response body: %v", err)
		return createErrorResponse("Failed to encode response", 500), nil
	}

	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    corsHeaders(),
		Body:       string(bodyBytes),
	}, nil
}

// handleDeleteFile physically deletes the S3 object
func handleDeleteFile(ctx context.Context, fileID string, bucketName string) (events.APIGatewayV2HTTPResponse, error) {
	if fileID == "" {
		return createErrorResponse("fileId is required", 400), nil
	}

	// Delete the object from S3
	_, err := s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(fileID),
	})
	if err != nil {
		log.Printf("Failed to delete object from S3: %v", err)
		return createErrorResponse(fmt.Sprintf("Failed to delete file: %v", err), 500), nil
	}

	// Return success message
	resBody := MessageResponseBody{
		Message: "Archivo eliminado de S3",
	}

	bodyBytes, err := json.Marshal(resBody)
	if err != nil {
		log.Printf("Failed to marshal response body: %v", err)
		return createErrorResponse("Failed to encode response", 500), nil
	}

	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    corsHeaders(),
		Body:       string(bodyBytes),
	}, nil
}

// handleOptions responds to CORS preflight requests
func handleOptions() (events.APIGatewayV2HTTPResponse, error) {
	resBody := MessageResponseBody{
		Message: "CORS OK",
	}

	bodyBytes, _ := json.Marshal(resBody)

	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    corsHeaders(),
		Body:       string(bodyBytes),
	}, nil
}

// createErrorResponse is a helper to format HTTP error responses
func createErrorResponse(message string, statusCode int) events.APIGatewayV2HTTPResponse {
	errBody := ErrorResponseBody{Error: message}
	bodyBytes, _ := json.Marshal(errBody)

	return events.APIGatewayV2HTTPResponse{
		StatusCode: statusCode,
		Headers:    corsHeaders(),
		Body:       string(bodyBytes),
	}
}

func main() {
	lambda.Start(Handler)
}
