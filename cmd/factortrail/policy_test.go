package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Structural rules from the P0 scope: no database, broker or order path, and
// the outbound calls are the JWKS fetch and the auto_trader retro/v1 provider. They are tests so a later change
// that adds a dependency or a client fails here, with the rule named.

var bannedDependencyFragments = []string{
	"database/sql",
	"github.com/jackc/",
	"github.com/lib/pq",
	"github.com/go-sql-driver/",
	"github.com/mattn/go-sqlite3",
	"modernc.org/sqlite",
	"github.com/redis/",
	"github.com/go-redis/",
	"github.com/segmentio/kafka-go",
	"github.com/confluentinc/",
	"github.com/IBM/sarama",
	"github.com/nats-io/",
	"github.com/rabbitmq/",
	"github.com/streadway/amqp",
	"go.mongodb.org/",
	"gorm.io/",
	"github.com/jmoiron/sqlx",
	"github.com/aws/aws-sdk-go",
	"cloud.google.com/",
	"google.golang.org/grpc",
}

// The generated provider client imports oapi-codegen's runtime, which pulls
// in github.com/google/uuid, and that imports the standard library's driver
// interface package. Those two packages hold interfaces only: they cannot open
// a connection, and database/sql itself, which could, stays banned.
var allowedStdlibInterfaces = map[string]bool{
	"database/sql/driver":   true,
	"database/sql/internal": true,
}

func TestNoDatabaseOrBrokerDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-C", "../..", "-deps", "./...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if allowedStdlibInterfaces[pkg] {
			continue
		}
		for _, banned := range bannedDependencyFragments {
			if strings.Contains(pkg, banned) {
				t.Errorf("dependency %s matches banned %q: P0 has no database, broker or cloud client", pkg, banned)
			}
		}
	}
}

var outboundCall = regexp.MustCompile(`http\.(Get|Post|Head|PostForm|DefaultClient|DefaultTransport|Client\{|NewRequest|NewRequestWithContext)|net\.(Dial|DialTimeout|Dialer)|exec\.Command`)

// Two places own outbound calls: internal/auth (the JWKS fetch) and
// internal/providers/autotrader (the retro/v1 upstream, with its generated
// client). Nothing else, including the MCP tools, opens a connection.
func TestOnlyAuthAndTheProviderMakeOutboundCalls(t *testing.T) {
	root := filepath.Join("..", "..")
	allowedFile := filepath.Join(root, "internal", "auth", "auth.go")
	allowedDir := filepath.Join(root, "internal", "providers", "autotrader") + string(filepath.Separator)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || path == allowedFile || strings.HasPrefix(path, allowedDir) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if m := outboundCall.Find(src); m != nil {
			t.Errorf("%s makes an outbound call (%s); only internal/auth (JWKS) and internal/providers/autotrader may", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
