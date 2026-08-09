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

// Handler is the entry point for the Lambda function.
// It inspects req.PathParameters["fileId"] to determine the flow:
//   - If fileId EXISTS  → Download flow (Burn After Reading)
//   - If fileId is ABSENT → Upload flow
func Handler(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	bucketName := os.Getenv("BUCKET_NAME")
	if bucketName == "" {
		log.Println("BUCKET_NAME environment variable is not set")
		return createErrorResponse("Internal server error configuration", 500), nil
	}

	// Determine flow based on the presence of fileId path parameter
	fileID := req.PathParameters["fileId"]

	if fileID != "" {
		// --- Download Flow (GET /download/{fileId}) - Burn After Reading ---
		return handleDownload(ctx, fileID, bucketName)
	}

	// --- Upload Flow (POST /upload) ---
	return handleUpload(ctx, bucketName)
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
// 1. Generates a presigned GET URL with 2-minute expiration
// 2. Deletes the DynamoDB record immediately
func handleDownload(ctx context.Context, fileID string, bucketName string) (events.APIGatewayProxyResponse, error) {
	// Generate a presigned GET URL with 2-minute expiration
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

	// Delete the record from DynamoDB immediately (Burn After Reading)
	_, err = dynamodbClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"fileId": &types.AttributeValueMemberS{Value: fileID},
		},
	})
	if err != nil {
		log.Printf("Failed to delete item from DynamoDB: %v", err)
		return createErrorResponse("Failed to process download", 500), nil
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
