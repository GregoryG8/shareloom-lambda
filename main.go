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

	s3Client := s3.NewFromConfig(cfg)
	s3PresignClient = s3.NewPresignClient(s3Client)
	dynamodbClient = dynamodb.NewFromConfig(cfg)
}

// ResponseBody defines the successful JSON response structure
type ResponseBody struct {
	UploadURL string `json:"uploadUrl"`
	FileID    string `json:"fileId"`
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

	now := time.Now()

	// 3. Generate a unique fileId using UUID
	fileID := uuid.New().String()

	// 4. Calculate a Unix timestamp expireAt for 24 hours in the future
	expireAt := now.Add(24 * time.Hour).Unix()

	// 5. Insert a record into Amazon DynamoDB
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

	// 6. Generate an S3 Pre-signed URL of type PUT lasting 15 minutes
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

	// 7. Respond with a JSON containing uploadUrl and fileId
	resBody := ResponseBody{
		UploadURL: presignReq.URL,
		FileID:    fileID,
	}

	bodyBytes, err := json.Marshal(resBody)
	if err != nil {
		log.Printf("Failed to marshal response body: %v", err)
		return createErrorResponse("Failed to encode response", 500), nil
	}

	// 8. Include CORS headers in the response
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

// 10. Include the call to lambda.Start(handler) in the main function
func main() {
	lambda.Start(Handler)
}
