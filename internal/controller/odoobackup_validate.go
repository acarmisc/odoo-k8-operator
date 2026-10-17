/*
Copyright 2026 Odoo K8s Operator.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	backupCronFieldCount = 5
	backupMinTimeout     = int64(60)
	backupMaxTimeout     = int64(86400)
	backupURLHTTP        = "http://"
	backupURLHTTPS       = "https://"
)

var (
	// backupPathRe is the charset of a subPath or an S3 prefix. The checks below reject
	// "..", leading slashes and empty segments on top of it.
	backupPathRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
	// backupDatabaseRe is the charset of a PostgreSQL database name as used here.
	backupDatabaseRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
	// backupBucketRe follows the S3 bucket naming rules that MinIO and AWS share.
	backupBucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// backupValidatePath rejects paths that escape the mount or are malformed.
func backupValidatePath(field, value string) error {
	if value == "" {
		return nil
	}
	if !backupPathRe.MatchString(value) {
		return fmt.Errorf("%s %q may only contain letters, digits, '.', '_', '-' and '/'", field, value)
	}
	if strings.HasPrefix(value, "/") {
		return fmt.Errorf("%s %q must not start with '/'", field, value)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(value, "/"), "/") {
		switch seg {
		case "":
			return fmt.Errorf("%s %q must not contain empty segments", field, value)
		case "..":
			return fmt.Errorf("%s %q must not contain '..'", field, value)
		}
	}
	return nil
}

// backupValidateSpec rejects specs that can never produce a meaningful backup. The CRD
// already checks most fields; this covers what it cannot express (cross-field rules,
// path semantics, quantities and the cron field count).
func backupValidateSpec(bk *odoov1.OdooBackup) error {
	if bk.Spec.InstanceRef.Name == "" {
		return errors.New("instanceRef.name is required")
	}
	if ns := bk.Spec.InstanceRef.Namespace; ns != "" && ns != bk.Namespace {
		return fmt.Errorf("instanceRef.namespace %q must be empty or the backup's namespace %q", ns, bk.Namespace)
	}
	if db := bk.Spec.Database; db != "" && !backupDatabaseRe.MatchString(db) {
		return fmt.Errorf("database %q may only contain letters, digits, '_', '.' and '-'", db)
	}
	if err := backupValidateDestination(&bk.Spec.Destination); err != nil {
		return err
	}
	if r := bk.Spec.Retention; r != nil && *r < 1 {
		return fmt.Errorf("retention must be at least 1, got %d", *r)
	}
	if bk.Spec.ScratchSize != "" {
		q, err := resource.ParseQuantity(bk.Spec.ScratchSize)
		if err != nil {
			return fmt.Errorf("scratchSize %q is not a quantity: %w", bk.Spec.ScratchSize, err)
		}
		if q.Sign() <= 0 {
			return fmt.Errorf("scratchSize %q must be positive", bk.Spec.ScratchSize)
		}
	}
	if t := bk.Spec.TimeoutSeconds; t != nil && (*t < backupMinTimeout || *t > backupMaxTimeout) {
		return fmt.Errorf("timeoutSeconds must be between %d and %d, got %d", backupMinTimeout, backupMaxTimeout, *t)
	}
	if bk.Spec.Schedule != "" {
		if n := len(strings.Fields(bk.Spec.Schedule)); n != backupCronFieldCount {
			return fmt.Errorf("schedule %q must have %d fields, got %d", bk.Spec.Schedule, backupCronFieldCount, n)
		}
	}
	return nil
}

// backupValidateDestination checks that exactly one destination is set and that its fields
// are usable.
func backupValidateDestination(d *odoov1.BackupDestination) error {
	if (d.PVC != nil) == (d.S3 != nil) {
		return errors.New("destination: exactly one of pvc or s3 must be set")
	}
	if d.PVC != nil {
		if d.PVC.ClaimName == "" {
			return errors.New("destination.pvc.claimName is required")
		}
		return backupValidatePath("destination.pvc.subPath", d.PVC.SubPath)
	}
	s3 := d.S3
	if !backupBucketRe.MatchString(s3.Bucket) {
		return fmt.Errorf("destination.s3.bucket %q is not a valid bucket name", s3.Bucket)
	}
	if err := backupValidatePath("destination.s3.prefix", s3.Prefix); err != nil {
		return err
	}
	if s3.Endpoint != "" && !strings.HasPrefix(s3.Endpoint, backupURLHTTP) && !strings.HasPrefix(s3.Endpoint, backupURLHTTPS) {
		return fmt.Errorf("destination.s3.endpoint %q must start with http:// or https://", s3.Endpoint)
	}
	if s3.CredentialsSecret == "" {
		return errors.New("destination.s3.credentialsSecret is required")
	}
	return nil
}
