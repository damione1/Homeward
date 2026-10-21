package backups_test

import (
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"testing"
)

func TestNHA156StrictSettingsAndCapabilities(t *testing.T) {
	id, _ := backups.ParseInstanceID("offsite")
	instance, _ := domain.ParseIntegrationInstanceID("backup-credentials")
	name, _ := storage.ParseSecretName("bundle")
	ref, _ := storage.NewSecretHandle(instance, name)
	good := func(endpoint, bucket, prefix, region string, handle storage.SecretHandle, limits backups.Limits) (backups.InstanceSettings, error) {
		return backups.NewS3Settings(id, true, endpoint, bucket, prefix, region, handle, limits)
	}
	s, err := good("https://s3.example.test", "nha156-bucket", "backup/house", "us-east-1", ref, backups.DefaultS3Limits())
	if err != nil || !s.Valid() || s.Kind() != backups.S3 {
		t.Fatal("S3 variant not admitted")
	}
	v, err := s.S3()
	if err != nil || v.Limits().ArtifactBytes() != 5<<30 || backups.DefaultLimits().ArtifactBytes() != 8<<30 {
		t.Fatal("backend limit changed common contract")
	}
	if _, err := s.Local(); err == nil {
		t.Fatal("cross-variant accessor")
	}
	if _, err := (backups.InstanceSettings{}).S3(); err == nil {
		t.Fatal("zero accessor")
	}
	for _, endpoint := range []string{"http://example.test", "https://user:secret@example.test", "https://example.test/path", "https://example.test?key=secret", "https://example.test#secret", "https://example.test/%2e%2e", "https://", "file:///tmp/file"} {
		if _, err := good(endpoint, "nha156-bucket", "backup/house", "us-east-1", ref, backups.DefaultS3Limits()); err == nil {
			t.Fatal("unsafe endpoint admitted")
		}
	}
	for _, bucket := range []string{"../other", "UPPERCASE", "arn:aws:s3:bucket", "192.168.0.11", "bad..name", "bucket--x-s3"} {
		if _, err := good("https://s3.example.test", bucket, "backup/house", "us-east-1", ref, backups.DefaultS3Limits()); err == nil {
			t.Fatal("unsafe bucket admitted")
		}
	}
	for _, prefix := range []string{"", "/root", "root/", "root//child", "root/../child", "root/%2e%2e", "root/./child", "root?secret"} {
		if _, err := good("https://s3.example.test", "nha156-bucket", prefix, "us-east-1", ref, backups.DefaultS3Limits()); err == nil {
			t.Fatal("unsafe prefix admitted")
		}
	}
	if _, err := good("https://s3.example.test", "nha156-bucket", "backup/house", "us-east-1", storage.SecretHandle{}, backups.DefaultS3Limits()); err == nil {
		t.Fatal("zero secret ref admitted")
	}
	if _, err := good("https://s3.example.test", "nha156-bucket", "backup/house", "us-east-1", ref, backups.DefaultLimits()); err == nil {
		t.Fatal("unadvertised single-PUT size admitted")
	}
}
