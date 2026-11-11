package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBlobRootHonorsTheOverride(t *testing.T) {
	t.Setenv("ODAC_MAIL_BLOB_DIR", "/srv/mail-objects")
	if got := blobRoot(); got != "/srv/mail-objects" {
		t.Fatalf("blobRoot() = %q with the override set", got)
	}
	t.Setenv("ODAC_MAIL_BLOB_DIR", "")
	if got := blobRoot(); !strings.HasSuffix(got, filepath.Join(".odac", "mail", "objects")) {
		t.Fatalf("blobRoot() = %q, want the .odac default", got)
	}
}
