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

// ResponseBody defines the successful JSON response structure for upload
type ResponseBody struct {
	UploadURL string `json:"uploadUrl"`
	FileID    string `json:"fileId"`
}

// DownloadResponseBody defines the successful JSON response for download
type DownloadResponseBody struct {
	DownloadURL string `json:"downloadUrl"`
}

// MessageResponseBody defines a generic message response
type MessageResponseBody struct {
	Message string `json:"message"`
}

// ErrorResponseBody defines the error JSON response structure
type ErrorResponseBody struct {
	Error string `json:"error"`
}

// Handler is the entry point for the Lambda function
func Handler(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	bucketName := os.Getenv("BUCKET_NAME")
	if bucketName == "" {
		log.Println("BUCKET_NAME environment variable is not set")
		return createErrorResponse("Internal server error configuration", 500), nil
	}

	// Route based on HTTP method and path
	path := req.Path
	method := req.HTTPMethod

	switch {
	case method == "GET" && strings.HasPrefix(path, "/download/"):
		return handleDownload(ctx, req, bucketName)
	case method == "DELETE" && strings.HasPrefix(path, "/file/"):
		return handleDeleteFile(ctx, req, bucketName)
	default:
		return handleUpload(ctx, req, bucketName)
	}
}

// handleUpload generates a presigned PUT URL and stores metadata in DynamoDB
func handleUpload(ctx context.Context, req events.APIGatewayProxyRequest, bucketName string) (events.APIGatewayProxyResponse, error) {
	now := time.Now()

	// Generate a unique fileId using UUID
	fileID := uuid.New().String()

	// Calculate a Unix timestamp expireAt for 24 hours in the future
	expireAt := now.Add(24 * time.Hour).Unix()

	// Insert a record into Amazon DynamoDB
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

	// Respond with a JSON containing uploadUrl and fileId
	resBody := ResponseBody{
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
// 1. Checks if fileId exists in DynamoDB
// 2. Generates a presigned GET URL (2 min expiry)
// 3. Deletes the DynamoDB record immediately
func handleDownload(ctx context.Context, req events.APIGatewayProxyRequest, bucketName string) (events.APIGatewayProxyResponse, error) {
	fileID := req.PathParameters["fileId"]
	if fileID == "" {
		return createErrorResponse("fileId is required", 400), nil
	}

	// Check if the fileId exists in DynamoDB
	getResult, err := dynamodbClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"fileId": &types.AttributeValueMemberS{Value: fileID},
		},
	})
	if err != nil {
		log.Printf("Failed to get item from DynamoDB: %v", err)
		return createErrorResponse("Internal server error", 500), nil
	}

	// If the item does not exist, return 404
	if getResult.Item == nil {
		return createErrorResponse("El archivo no existe o ya fue autodestruido", 404), nil
	}

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

	// Delete the record from DynamoDB immediately (burn after reading)
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

// handleDeleteFile physically deletes the S3 object
func handleDeleteFile(ctx context.Context, req events.APIGatewayProxyRequest, bucketName string) (events.APIGatewayProxyResponse, error) {
	fileID := req.PathParameters["fileId"]
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
		Message: "Archivo eliminado exitosamente",
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
