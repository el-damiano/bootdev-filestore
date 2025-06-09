package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (cfg apiConfig) ensureAssetsDir() error {
	if _, err := os.Stat(cfg.assetsRoot); os.IsNotExist(err) {
		return os.Mkdir(cfg.assetsRoot, 0755)
	}
	return nil
}

func getAssetPath(mediaType string) string {
	assetID := make([]byte, 32)
	_, err := rand.Read(assetID)
	if err != nil {
		panic("failed to generate random bytes")
	}

	assetIDString := base64.URLEncoding.EncodeToString(assetID)
	ext := mediaTypeToExt(mediaType)
	return fmt.Sprintf("%s%s", assetIDString, ext)
}

func (cfg apiConfig) getObjectURL(key string) string {
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, key)
}

func (cfg apiConfig) getAssetDiskPath(assetPath string) string {
	return filepath.Join(cfg.assetsRoot, assetPath)
}

func (cfg apiConfig) getAssetURL(assetPath string) string {
	return fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, assetPath)
}

func (cfg apiConfig) getAssetDiskPathFromURL(assetURL string) (string, error) {
	urlBase := fmt.Sprintf("http://localhost:%s/", cfg.port)
	_, assetDiskPath, ok := strings.Cut(assetURL, urlBase)
	if !ok {
		return "", fmt.Errorf("Invalid asset URL. Missing expected base URL")
	}
	return assetDiskPath, nil
}

func mediaTypeToExt(mediaType string) string {
	parts := strings.Split(mediaType, "/")
	if len(parts) != 2 {
		return ".bin"
	}
	return "." + parts[1]
}
