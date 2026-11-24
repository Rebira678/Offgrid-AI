//go:build !unix

package blobstore

import "errors"

func freeBytes(string) (uint64, error) {
	return 0, errors.New("blobstore: free space is not measured on this platform")
}
