//go:build !unix

package treebackup

import "errors"

func statIdentity(string) (uint64, uint64, error) {
	return 0, 0, errors.New("tree backups need a unix host")
}
