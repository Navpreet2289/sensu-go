package etcd

import (
	"context"
	"fmt"
	"path"
	"strconv"
)

const (
	keepalivesPathPrefix = "keepalives"
)

func getKeepalivePath(org, id string) string {
	return path.Join(etcdRoot, keepalivesPathPrefix, org, id)
}

func (s *etcdStore) GetKeepalive(org, entityID string) (int64, error) {
	// Verify that the organization exist
	if _, err := s.GetOrganizationByName(org); err != nil {
		return 0, fmt.Errorf("the organization '%s' is invalid", org)
	}

	resp, err := s.client.Get(context.Background(), getKeepalivePath(org, entityID))
	if err != nil {
		return 0, err
	}

	if len(resp.Kvs) == 0 {
		return 0, nil
	}

	expirationStr := string(resp.Kvs[0].Value)
	expiration, err := strconv.ParseInt(expirationStr, 10, 64)
	if err != nil {
		return 0, err
	}

	return expiration, nil
}

func (s *etcdStore) UpdateKeepalive(org, entityID string, expiration int64) error {
	// Verify that the organization exist
	if _, err := s.GetOrganizationByName(org); err != nil {
		return fmt.Errorf("the organization '%s' is invalid", org)
	}

	expirationStr := strconv.FormatInt(expiration, 10)
	_, err := s.client.Put(context.Background(), getKeepalivePath(org, entityID), expirationStr)
	return err
}
