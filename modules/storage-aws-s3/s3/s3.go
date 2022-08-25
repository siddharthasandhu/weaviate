//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2022 SeMI Technologies B.V. All rights reserved.
//
//  CONTACT: hello@semi.technology
//

package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pkg/errors"
	"github.com/semi-technologies/weaviate/entities/snapshots"
	"github.com/sirupsen/logrus"
)

const (
	AWS_ROLE_ARN                = "AWS_ROLE_ARN"
	AWS_WEB_IDENTITY_TOKEN_FILE = "AWS_WEB_IDENTITY_TOKEN_FILE"
	AWS_REGION                  = "AWS_REGION"
	AWS_DEFAULT_REGION          = "AWS_DEFAULT_REGION"
)

type s3 struct {
	client   *minio.Client
	config   Config
	logger   logrus.FieldLogger
	dataPath string
}

func New(config Config, logger logrus.FieldLogger, dataPath string) (*s3, error) {
	region := os.Getenv(AWS_REGION)
	if len(region) == 0 {
		region = os.Getenv(AWS_DEFAULT_REGION)
	}
	creds := credentials.NewEnvAWS()
	if len(os.Getenv(AWS_WEB_IDENTITY_TOKEN_FILE)) > 0 && len(os.Getenv(AWS_ROLE_ARN)) > 0 {
		creds = credentials.NewIAM("")
	}
	client, err := minio.New(config.Endpoint(), &minio.Options{
		Creds:  creds,
		Region: region,
		Secure: config.UseSSL(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "create client")
	}
	return &s3{client, config, logger, dataPath}, nil
}

func (s *s3) makeObjectName(parts ...string) string {
	base := path.Join(parts...)
	return path.Join(s.config.SnapshotRoot(), base)
}

func makeFilePath(parts ...string) string {
	return path.Join(parts...)
}

func (s *s3) StoreSnapshot(ctx context.Context, snapshot *snapshots.Snapshot) error {
	// create bucket
	bucketName := s.config.BucketName()
	bucketExists, err := s.client.BucketExists(ctx, bucketName)
	if err != nil {
		return snapshots.NewErrInternal(errors.Wrap(err, "check bucket exists"))
	}
	if !bucketExists {
		err = s.client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
		if err != nil {
			return snapshots.NewErrInternal(
				errors.Wrapf(err, "create bucket '%s'", bucketName))
		}
	}
	// save files
	putOptions := minio.PutObjectOptions{ContentType: "application/octet-stream"}
	for _, srcRelPath := range snapshot.Files {
		if err := ctx.Err(); err != nil {
			return snapshots.NewErrContextExpired(
				errors.Wrap(err, "store snapshot aborted"))
		}

		objectName := s.makeObjectName(snapshot.ClassName, snapshot.ID, srcRelPath)
		filePath := makeFilePath(s.dataPath, srcRelPath)

		_, err := s.client.FPutObject(ctx, bucketName, objectName, filePath, putOptions)
		if err != nil {
			return snapshots.NewErrInternal(
				errors.Wrapf(err, "put file '%s'", objectName))
		}
	}

	return s.putMeta(ctx, snapshot)
}

func (s *s3) putMeta(ctx context.Context, snapshot *snapshots.Snapshot) error {
	content, err := json.Marshal(snapshot)
	if err != nil {
		return snapshots.NewErrInternal(errors.Wrap(err, "save meta"))
	}
	objectName := s.makeObjectName(snapshot.ClassName, snapshot.ID, "snapshot.json")
	reader := bytes.NewReader(content)
	_, err = s.client.PutObject(ctx, s.config.BucketName(), objectName, reader, reader.Size(),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return snapshots.NewErrInternal(
			errors.Wrapf(err, "put file '%s'", objectName))
	}
	return nil
}

func (s *s3) RestoreSnapshot(ctx context.Context, className, snapshotID string) (*snapshots.Snapshot, error) {
	bucketName := s.config.BucketName()
	bucketExists, err := s.client.BucketExists(ctx, bucketName)
	if !bucketExists {
		return nil, errors.Wrap(err, "backup bucket does not exist")
	}
	if err != nil {
		return nil, errors.Wrap(err, "can't connect to bucket")
	}

	// Load the metadata from the backup into a snapshot struct
	snapshot, err := s.getSnapshotFromBucket(ctx, className, snapshotID)
	if err != nil {
		return nil, errors.Wrap(err, "restore snapshot")
	}

	// Restore the files
	for _, srcRelPath := range snapshot.Files {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrapf(err, "restore snapshot aborted")
		}

		// Get the correct paths for the backup file and the active file
		objectName := s.makeObjectName(className, snapshotID, srcRelPath)
		filePath := makeFilePath(s.dataPath, srcRelPath)

		// Download the backup file from the bucket
		err := s.client.FGetObject(ctx, s.config.BucketName(), objectName, filePath, minio.GetObjectOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, "Unable to restore file %s, system might be in a corrupted state", filePath)
		}
	}
	return snapshot, nil
}

func (s *s3) GetMeta(ctx context.Context, className, snapshotID string) (*snapshots.Snapshot, error) {
	snapshot, err := s.getSnapshotFromBucket(ctx, className, snapshotID)
	if err != nil {
		return nil, err
	}

	return snapshot, nil
}

func (s *s3) SetMetaError(ctx context.Context, className, snapshotID string, snapErr error) error {
	snapshot, err := s.getSnapshotFromBucket(ctx, className, snapshotID)
	if err != nil {
		return errors.Wrap(err, "set snapshot error")
	}

	snapshot.Status = string(snapshots.CreateFailed)
	snapshot.Error = snapErr.Error()

	return s.putMeta(ctx, snapshot)
}

func (s *s3) SetMetaStatus(ctx context.Context, className, snapshotID, status string) error {
	snapshot, err := s.getSnapshotFromBucket(ctx, className, snapshotID)
	if err != nil {
		return errors.Wrap(err, "set snapshot status")
	}

	if status == string(snapshots.CreateSuccess) {
		snapshot.CompletedAt = time.Now()
	}

	snapshot.Status = status

	return s.putMeta(ctx, snapshot)
}

func (s *s3) InitSnapshot(ctx context.Context, className, snapshotID string) (*snapshots.Snapshot, error) {
	if err := s.client.MakeBucket(ctx, s.config.BucketName(), minio.MakeBucketOptions{}); err != nil {
		s3Err, ok := err.(minio.ErrorResponse)
		if ok && s3Err.StatusCode == http.StatusConflict {
			// bucket has already been created
		} else {
			return nil, errors.Wrap(err, "init snapshot")
		}
	}

	snapshot := snapshots.New(className, snapshotID, time.Now())
	snapshot.Status = string(snapshots.CreateStarted)

	if err := s.putMeta(ctx, snapshot); err != nil {
		return nil, errors.Wrap(err, "init snapshot")
	}

	return snapshot, nil
}

func (s *s3) DestinationPath(className, snapshotID string) string {
	return "s3://" + path.Join(s.config.BucketName(),
		s.makeObjectName(className, snapshotID, "snapshot.json"))
}

func (s *s3) getSnapshotFromBucket(ctx context.Context, className, snapshotID string) (*snapshots.Snapshot, error) {
	objectName := s.makeObjectName(className, snapshotID, "snapshot.json")
	obj, err := s.client.GetObject(ctx, s.config.BucketName(), objectName, minio.GetObjectOptions{})
	if err != nil {
		return nil, snapshots.NewErrInternal(
			errors.Wrapf(err, "get file '%s'", objectName))
	}

	data, err := io.ReadAll(obj)
	s3Err, ok := err.(minio.ErrorResponse)
	if err != nil && ok && s3Err.StatusCode == http.StatusNotFound {
		return nil, snapshots.ErrNotFound{}
	} else if err != nil {
		return nil, snapshots.NewErrInternal(
			errors.Wrapf(err, "read file '%s'", objectName))
	}

	var snapshot snapshots.Snapshot
	err = json.Unmarshal(data, &snapshot)
	if err != nil {
		return nil, errors.Wrapf(err, "unmarshal meta")
	}

	return &snapshot, nil
}