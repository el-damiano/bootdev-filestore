package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	if video.UserID != userID {
		respondWithJSON(w, http.StatusUnauthorized, "Insufficient rights to upload the video")
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type, only MP4 supported.", nil)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	const fileTmpPath = "tubely-upload.mp4"
	fileTmp, err := os.CreateTemp("", fileTmpPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
	}
	defer os.Remove(fileTmp.Name())
	defer fileTmp.Close()

	_, err = io.Copy(fileTmp, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video to disk", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(fileTmp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't calculate aspect ratio", err)
		return
	}

	prefixKey := "other"
	if aspectRatio == "16:9" {
		prefixKey = "landscape"
	} else if aspectRatio == "9:16" {
		prefixKey = "portrait"
	}

	fileKey := getAssetPath(mediaType)
	fileKey = filepath.Join(prefixKey, fileKey)

	fileProcessedPath, err := processVideoForFastStart(fileTmp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}
	defer os.Remove(fileProcessedPath)

	fileProcessed, err := os.Open(fileProcessedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video", err)
		return
	}
	defer fileProcessed.Close()

	params := s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(fileKey),
		Body:        fileProcessed,
		ContentType: aws.String(mediaType),
	}

	_, err = cfg.s3Client.PutObject(context.Background(), &params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading file to S3", err)
		return
	}

	fileURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, fileKey)
	video.VideoURL = &fileURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	videoPresigned, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create presigned URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoPresigned)
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, fmt.Errorf("Missing video url for video %d", video.ID)
	}

	vidURL := strings.Split(*video.VideoURL, ",")
	if len(vidURL) < 2 {
		return video, errors.New("Invalid Video URL, expected format <bucket>,<key>")
	}
	bucket := vidURL[0]
	key := vidURL[1]

	urlPresigned, err := generatePresignedURL(cfg.s3Client, bucket, key, 24*time.Hour)
	if err != nil {
		return video, err
	}

	video.VideoURL = &urlPresigned
	return video, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v",
		"error",
		"-print_format",
		"json",
		"-show_streams",
		filePath)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var videoInfo struct {
		Streams []struct {
			Width  int `json:"width,omitempty"`
			Height int `json:"height,omitempty"`
		} `json:"streams"`
	}

	err = json.Unmarshal(stdout.Bytes(), &videoInfo)
	if err != nil {
		return "", fmt.Errorf("Couldn't parse ffprobe output: %v", err)
	}

	if len(videoInfo.Streams) == 0 {
		return "", errors.New("No video streams found")
	}

	width := videoInfo.Streams[0].Width
	height := videoInfo.Streams[0].Height

	sizeRatio := float64(width) / float64(height)
	if math.Abs(sizeRatio-1.777) < 0.2 {
		return "16:9", nil
	} else if math.Abs(sizeRatio-0.5625) < 0.2 {
		return "9:16", nil
	} else {
		return "other", nil
	}

}

func processVideoForFastStart(filepath string) (string, error) {
	newPath := filepath + ".processing"

	cmd := exec.Command(
		"ffmpeg",
		"-i",
		filepath,
		"-c",
		"copy",
		"-movflags",
		"faststart",
		"-f",
		"mp4",
		newPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(newPath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed video: %v", err)
	}

	if fileInfo.Size() < 1 {
		return "", errors.New("processed video is empty")
	}

	return newPath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	client := s3.NewPresignClient(s3Client)
	params := s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	presignedGetObject, err := client.PresignGetObject(
		context.Background(),
		&params,
		s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}
	return presignedGetObject.URL, nil
}
