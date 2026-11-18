package service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	bizerrors "yagp/internal/errors"
	"yagp/internal/model"
	"yagp/internal/repository"
	"yagp/pkg/logger"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestBackupUsesWALSnapshotAndUniquePublishedFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	requireServiceError(t, logger.Init(&logger.Config{Level: "error"}), nil)
	loadServiceConfig(t, "database:\n  path: live.db\n")
	db, err := gorm.Open(sqlite.Open("live.db"), &gorm.Config{})
	requireServiceError(t, err, nil)
	sqlDB, err := db.DB()
	requireServiceError(t, err, nil)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	requireServiceError(t, db.Exec("PRAGMA journal_mode=WAL").Error, nil)
	requireServiceError(t, db.Exec("PRAGMA wal_autocheckpoint=0").Error, nil)
	requireServiceError(t, db.AutoMigrate(&model.SystemConfig{}), nil)
	backup := NewBackupService(db)
	repo := repository.NewSystemConfigRepository(backup.db)
	settings, err := repo.Get()
	requireServiceError(t, err, nil)
	settings.BackupRetentionCount = 10
	requireServiceError(t, repo.Update(settings), nil)
	requireServiceError(t, db.Exec("CREATE TABLE snapshot_value (value TEXT NOT NULL)").Error, nil)
	requireServiceError(t, db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error, nil)
	requireServiceError(t, db.Exec("INSERT INTO snapshot_value VALUES ('first committed WAL value')").Error, nil)
	wal, err := os.Stat("live.db-wal")
	requireServiceError(t, err, nil)
	if wal.Size() == 0 {
		t.Fatal("fixture did not retain committed data in WAL")
	}

	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		requireServiceError(t, backup.CreateBackup(t.Context()), nil)
		synctest.Wait()
		first, err := filepath.Glob("backups/gost_panel_*.db")
		requireServiceError(t, err, nil)
		if len(first) != 1 {
			t.Fatalf("first snapshot = %v", first)
		}
		before, err := os.ReadFile(first[0])
		requireServiceError(t, err, nil)
		requireServiceError(t, db.Exec("UPDATE snapshot_value SET value = 'second committed WAL value'").Error, nil)
		requireServiceError(t, backup.CreateBackup(t.Context()), nil)
		synctest.Wait()
		if !time.Now().Equal(started) {
			t.Fatal("backups did not run at the same timestamp")
		}
		files, err := filepath.Glob("backups/gost_panel_*.db")
		requireServiceError(t, err, nil)
		if len(files) != 2 {
			t.Fatalf("same-time snapshots collided: %v", files)
		}
		for _, filename := range files {
			snapshot, err := gorm.Open(sqlite.Open(filename), &gorm.Config{})
			requireServiceError(t, err, nil)
			connection, err := snapshot.DB()
			requireServiceError(t, err, nil)
			var value, integrity string
			requireServiceError(t, snapshot.Raw("SELECT value FROM snapshot_value").Scan(&value).Error, nil)
			requireServiceError(t, snapshot.Raw("PRAGMA integrity_check").Scan(&integrity).Error, nil)
			requireServiceError(t, connection.Close(), nil)
			want := "second committed WAL value"
			if filename == first[0] {
				want = "first committed WAL value"
			}
			if value != want || integrity != "ok" {
				t.Fatalf("snapshot %s = %q (%s), want %q", filename, value, integrity, want)
			}
		}

		// VACUUM cannot run in a transaction. The main database file exists, but
		// copying it would silently discard the committed WAL values above.
		tx := db.Begin()
		requireServiceError(t, tx.Error, nil)
		requireServiceError(t, NewBackupService(tx).CreateBackup(t.Context()), bizerrors.ErrBackupFailed)
		requireServiceError(t, tx.Rollback().Error, nil)
		after, err := os.ReadFile(first[0])
		requireServiceError(t, err, nil)
		if string(after) != string(before) {
			t.Fatal("failed backup overwrote an existing snapshot")
		}
		entries, err := os.ReadDir("backups")
		requireServiceError(t, err, nil)
		if len(entries) != 2 {
			t.Fatalf("failed backup published a snapshot or left temporary files: %v", entries)
		}
	})
}

func TestBackupRetentionOrdersLegacyAndFractionalSnapshots(t *testing.T) {
	dir := t.TempDir()
	backup := NewBackupService(newServiceTestDB(t))
	repo := repository.NewSystemConfigRepository(backup.db)
	settings, err := repo.Get()
	requireServiceError(t, err, nil)
	settings.BackupRetentionCount = 1
	requireServiceError(t, repo.Update(settings), nil)

	older := filepath.Join(dir, "gost_panel_20250131_120000.db")
	newer := filepath.Join(dir, "gost_panel_20250131_120000.000000001-17.db")
	invalidDate := filepath.Join(dir, "gost_panel_20250231_120000.db")
	for _, name := range []string{older, newer, invalidDate} {
		requireServiceError(t, os.WriteFile(name, []byte("snapshot"), 0o600), nil)
	}

	backup.cleanOldBackups(t.Context(), dir)
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("older legacy snapshot was retained: %v", err)
	}
	for _, name := range []string{newer, invalidDate} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("missing file %s: %v", name, err)
		}
	}
}

func TestAutomaticBackupShutdownCancelsPendingSnapshot(t *testing.T) {
	t.Chdir(t.TempDir())
	loadServiceConfig(t, "database:\n  path: live.db\n")
	db := newServiceTestDB(t)
	saveServiceFixtures(t, db, &model.SystemConfig{ID: 1, AutoBackup: true})
	raw, err := db.DB()
	requireServiceError(t, err, nil)

	// Occupy the sole SQLite connection after the scheduled backup creates its
	// temporary file, just before VACUUM INTO requests the connection.
	held := make(chan *sql.Conn, 1)
	requireServiceError(t, db.Callback().Raw().Before("gorm:raw").Register("hold_snapshot_connection", func(tx *gorm.DB) {
		conn, err := raw.Conn(t.Context())
		if err != nil {
			tx.AddError(err)
			return
		}
		held <- conn
	}), nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewBackupService(db).Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case conn := <-held:
		defer conn.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("scheduled backup did not reach VACUUM INTO")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduled backup still waiting for the connection after shutdown")
	}
	entries, err := os.ReadDir("backups")
	requireServiceError(t, err, nil)
	if len(entries) != 0 {
		t.Fatalf("interrupted backup left files: %v", entries)
	}
}
