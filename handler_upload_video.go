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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
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

	_, err = fileTmp.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset file pointer", err)
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

	params := s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(fileKey),
		Body:        fileTmp,
		ContentType: aws.String(mediaType),
	}

	_, err = cfg.s3Client.PutObject(context.Background(), &params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading file to S3", err)
		return
	}

	fileURL := cfg.getObjectURL(fileKey)
	video.VideoURL = &fileURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
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
