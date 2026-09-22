package http

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"hash/crc32"
	stdhttp "net/http"

	"github.com/can3p/tommy/plugins/s3"
)

func validateChecksums(headers stdhttp.Header, body []byte) *s3Error {
	if value := headers.Get("Content-MD5"); value != "" {
		digest := md5.Sum(body)
		if err := compareDigest(value, digest[:]); err != nil {
			if err == errInvalidDigest {
				return &s3Error{"InvalidDigest", "The Content-MD5 you specified is not valid.", stdhttp.StatusBadRequest, err, ""}
			}
			return &s3Error{"BadDigest", "The Content-MD5 you specified did not match what we received.", stdhttp.StatusBadRequest, err, ""}
		}
	}
	checks := []struct {
		header string
		sum    func([]byte) []byte
	}{
		{"x-amz-checksum-crc32", func(data []byte) []byte {
			value := crc32.ChecksumIEEE(data)
			return []byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
		}},
		{"x-amz-checksum-crc32c", func(data []byte) []byte {
			value := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
			return []byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
		}},
		{"x-amz-checksum-sha1", func(data []byte) []byte { value := sha1.Sum(data); return value[:] }},
		{"x-amz-checksum-sha256", func(data []byte) []byte { value := sha256.Sum256(data); return value[:] }},
	}
	for _, check := range checks {
		if value := headers.Get(check.header); value != "" {
			if err := compareDigest(value, check.sum(body)); err != nil {
				if err == errInvalidDigest {
					return &s3Error{"InvalidDigest", "The checksum you specified is not valid.", stdhttp.StatusBadRequest, err, ""}
				}
				return &s3Error{"BadDigest", "The checksum you specified did not match what we received.", stdhttp.StatusBadRequest, err, ""}
			}
		}
	}
	return nil
}

func compareDigest(encoded string, expected []byte) error {
	actual, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(actual) != len(expected) {
		return errInvalidDigest
	}
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return errChecksumMismatch
	}
	return nil
}

type checksumError string

func (e checksumError) Error() string { return string(e) }

const (
	errInvalidDigest    checksumError = "invalid base64 checksum"
	errChecksumMismatch checksumError = "checksum mismatch"
)

func checksumsFromHeaders(headers stdhttp.Header) s3.Checksums {
	return s3.Checksums{
		CRC32: headers.Get("x-amz-checksum-crc32"), CRC32C: headers.Get("x-amz-checksum-crc32c"),
		SHA1: headers.Get("x-amz-checksum-sha1"), SHA256: headers.Get("x-amz-checksum-sha256"),
	}
}

func writeChecksumHeaders(headers stdhttp.Header, checksums s3.Checksums) {
	if checksums.CRC32 != "" {
		headers.Set("x-amz-checksum-crc32", checksums.CRC32)
	}
	if checksums.CRC32C != "" {
		headers.Set("x-amz-checksum-crc32c", checksums.CRC32C)
	}
	if checksums.SHA1 != "" {
		headers.Set("x-amz-checksum-sha1", checksums.SHA1)
	}
	if checksums.SHA256 != "" {
		headers.Set("x-amz-checksum-sha256", checksums.SHA256)
	}
}

func copyChecksumHeaders(destination, source stdhttp.Header) {
	for _, key := range []string{"x-amz-checksum-crc32", "x-amz-checksum-crc32c", "x-amz-checksum-sha1", "x-amz-checksum-sha256"} {
		if value := source.Get(key); value != "" {
			destination.Set(key, value)
		}
	}
}
