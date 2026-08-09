package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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

// Handler is the entry point for the Lambda function.
// Compatible with API Gateway v1 and v2 payloads.
// Routes based on the HTTP method:
//   - POST   → Upload flow
//   - GET    → Download flow (Burn After Reading)
//   - DELETE → Delete file physically from S3
func Handler(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	bucketName := os.Getenv("BUCKET_NAME")
	if bucketName == "" {
		log.Println("BUCKET_NAME environment variable is not set")
		return createErrorResponse("Internal server error configuration", 500), nil
	}

	// Detect HTTP method — compatible with API Gateway v1 and v2 payloads
	method := req.HTTPMethod
	if method == "" {
		method = req.RequestContext.HTTPMethod
	}

	// Extract fileId path parameter
	fileID, hasFileID := req.PathParameters["fileId"]

	switch {
	case method == "POST":
		return handleUpload(ctx, bucketName)
	case method == "GET" && hasFileID && fileID != "":
		return handleDownload(ctx, fileID, bucketName)
	case method == "DELETE" && hasFileID && fileID != "":
		return handleDeleteFile(ctx, fileID, bucketName)
	default:
		return createErrorResponse("Method not allowed", 405), nil
	}
}

// handleUpload generates a presigned PUT URL and stores metadata in DynamoDB.
// Uses UUID v4 as the fileId.
func handleUpload(ctx context.Context, bucketName string) (events.APIGatewayProxyResponse, error) {
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

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Access-Control-Allow-Origin": "*",
			"Content-Type":                "application/json",
		},
		Body: string(bodyBytes),
	}, nil
}

// handleDownload implements the Burn After Reading pattern:
// 1. Attempts a conditional DeleteItem (attribute_exists(fileId)) to atomically verify and destroy the record
// 2. If the delete fails (record doesn't exist), returns 404
// 3. Only on successful delete, generates a presigned GET URL with 2-minute expiration
func handleDownload(ctx context.Context, fileID string, bucketName string) (events.APIGatewayProxyResponse, error) {
	// Attempt conditional delete — this atomically checks existence and removes the record
	_, err := dynamodbClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"fileId": &types.AttributeValueMemberS{Value: fileID},
		},
		ConditionExpression: aws.String("attribute_exists(fileId)"),
	})
	if err != nil {
		// ConditionalCheckFailedException means the item didn't exist (already burned or expired)
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

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Access-Control-Allow-Origin": "*",
			"Content-Type":                "application/json",
		},
		Body: string(bodyBytes),
	}, nil
}

// handleDeleteFile physically deletes the S3 object
func handleDeleteFile(ctx context.Context, fileID string, bucketName string) (events.APIGatewayProxyResponse, error) {
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

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Access-Control-Allow-Origin": "*",
			"Content-Type":                "application/json",
		},
		Body: string(bodyBytes),
	}, nil
}

// createErrorResponse is a helper to format HTTP error responses
func createErrorResponse(message string, statusCode int) events.APIGatewayProxyResponse {
	errBody := ErrorResponseBody{Error: message}
	bodyBytes, _ := json.Marshal(errBody)

	return events.APIGatewayProxyResponse{
		StatusCode: statusCode,
		Headers: map[string]string{
			"Access-Control-Allow-Origin": "*",
			"Content-Type":                "application/json",
		},
		Body: string(bodyBytes),
	}
}

func main() {
	lambda.Start(Handler)
}
