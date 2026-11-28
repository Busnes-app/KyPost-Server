package config

import (
	"errors"
	"os"
	"strings"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/ky-primitives/s3"
)

// S3Vars configure the multi-mode bucket for message bodies
// (docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md, "MH11 object storage,
// amended"), as KyDrive's KYDRIVE_S3_*.
var S3Vars = []string{"KYPOST_S3_ENDPOINT", "KYPOST_S3_BUCKET", "KYPOST_S3_REGION", "KYPOST_S3_ACCESS_KEY", "KYPOST_S3_SECRET_KEY", "KYPOST_S3_SECRET_KEY_FILE"}

func s3Set() bool {
	for _, k := range S3Vars {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// ObjectStorage reads KYPOST_S3_* for a multi start: the endpoint, bucket, access key and a
// secret (inline or from a private file, not two that differ) are required; the region
// defaults to us-east-1. The client checks the endpoint is HTTPS. No error repeats a value.
func ObjectStorage() (*s3.Client, error) {
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	secret := os.Getenv("KYPOST_S3_SECRET_KEY")
	if file := get("KYPOST_S3_SECRET_KEY_FILE"); file != "" {
		fromFile, err := applysetup.Secret(file)
		if err != nil {
			return nil, errors.New("KYPOST_S3_SECRET_KEY_FILE must be a private regular file")
		}
		if secret != "" && secret != fromFile {
			return nil, errors.New("KYPOST_S3_SECRET_KEY and KYPOST_S3_SECRET_KEY_FILE differ")
		}
		secret = fromFile
	}
	if get("KYPOST_S3_ENDPOINT") == "" || get("KYPOST_S3_BUCKET") == "" || get("KYPOST_S3_ACCESS_KEY") == "" || secret == "" {
		return nil, errors.New("KYPOST_TENANCY=multi requires object storage: KYPOST_S3_ENDPOINT, KYPOST_S3_BUCKET, KYPOST_S3_ACCESS_KEY and KYPOST_S3_SECRET_KEY or KYPOST_S3_SECRET_KEY_FILE")
	}
	c, err := s3.New(s3.Config{Endpoint: get("KYPOST_S3_ENDPOINT"), Bucket: get("KYPOST_S3_BUCKET"), Region: get("KYPOST_S3_REGION"), AccessKey: get("KYPOST_S3_ACCESS_KEY"), SecretKey: secret})
	if err != nil {
		return nil, errors.New("KYPOST_S3_ENDPOINT must be an https URL without credentials, query or fragment, and KYPOST_S3_BUCKET a valid bucket name")
	}
	return c, nil
}
