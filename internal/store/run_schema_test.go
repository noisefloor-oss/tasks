package store

import (
	"strings"
	"testing"
)

func validRunStateDB(t *testing.T) *DB {
	t.Helper()
	db, _ := newDB(t)
	taskID := addTask(t, db, "main", "run target")
	err := db.transact("BEGIN IMMEDIATE", func(tx *tx) error {
		if _, err := tx.Exec(`UPDATE meta SET value='2' WHERE key='fencing'`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO runs(
			run_id,tag,task_id,owner,claim_acquisition_id,claim_token,status,current_attempt,
			start_request_id,terminal_result_digest,force_cancel_fence,started_at,updated_at,terminal_at,extra)
			VALUES (1,'main',?,'agent-a','00112233445566778899aabbccddeeff',1,
			'running',1,'req.start',NULL,NULL,'2026-08-28T17:45:00Z','2026-08-28T17:45:00Z',NULL,NULL)`, taskID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO attempts(
			run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
			created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
			VALUES (1,1,'11223344556677889900aabbccddeeff',2,'running',900,1,
			'req.start','2026-08-28T17:45:00Z','2026-08-28T18:00:00Z',
			'2026-08-28T17:45:00Z',NULL,NULL,NULL)`)
		return err
	})
	if err != nil {
		t.Fatalf("insert valid run fixture: %v", err)
	}
	return db
}

func TestValidateRunStateCrossRowInvariants(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		if err := validRunStateDB(t).ValidateRunState(); err != nil {
			t.Fatal(err)
		}
	})

	tests := []struct {
		name   string
		want   string
		mutate func(*testing.T, *DB)
	}{
		{
			name: "current not greatest",
			want: "current attempt is greatest",
			mutate: func(t *testing.T, db *DB) {
				_, err := db.db.Exec(`INSERT INTO attempts(
					run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
					created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
					VALUES (1,2,'21223344556677889900aabbccddeeff',3,'lost',900,1,
					'req.two','2026-08-28T17:45:00Z','2026-08-28T18:00:00Z',
					'2026-08-28T18:01:00Z','2026-08-28T18:01:00Z',NULL,NULL)`)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "attempt gap",
			want: "attempt numbers are contiguous",
			mutate: func(t *testing.T, db *DB) {
				err := db.transact("BEGIN IMMEDIATE", func(tx *tx) error {
					if _, err := tx.Exec(`UPDATE attempts SET status='lost',terminal_at='2026-08-28T18:01:00Z' WHERE run_id=1 AND attempt_no=1`); err != nil {
						return err
					}
					if _, err := tx.Exec(`INSERT INTO attempts(
						run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
						created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
						VALUES (1,3,'31223344556677889900aabbccddeeff',3,'running',900,1,
						'req.three','2026-08-28T18:01:00Z','2026-08-28T18:16:00Z',
						'2026-08-28T18:01:00Z',NULL,NULL,NULL)`); err != nil {
						return err
					}
					_, err := tx.Exec(`UPDATE runs SET current_attempt=3 WHERE run_id=1`)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "incoherent current status",
			want: "statuses are coherent",
			mutate: func(t *testing.T, db *DB) {
				if _, err := db.db.Exec(`UPDATE runs SET status='retryable' WHERE run_id=1`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "noncurrent succeeded",
			want: "non-current attempts are failed or lost",
			mutate: func(t *testing.T, db *DB) {
				err := db.transact("BEGIN IMMEDIATE", func(tx *tx) error {
					if _, err := tx.Exec(`UPDATE attempts SET status='succeeded',terminal_at='2026-08-28T18:01:00Z' WHERE run_id=1 AND attempt_no=1`); err != nil {
						return err
					}
					if _, err := tx.Exec(`INSERT INTO attempts(
						run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
						created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
						VALUES (1,2,'41223344556677889900aabbccddeeff',3,'running',900,1,
						'req.successor','2026-08-28T18:01:00Z','2026-08-28T18:16:00Z',
						'2026-08-28T18:01:00Z',NULL,NULL,NULL)`); err != nil {
						return err
					}
					_, err := tx.Exec(`UPDATE runs SET current_attempt=2 WHERE run_id=1`)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "bad timestamp",
			want: "non-RFC3339 timestamp",
			mutate: func(t *testing.T, db *DB) {
				if _, err := db.db.Exec(`UPDATE runs SET started_at='yesterday' WHERE run_id=1`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "counter below fence",
			want: "below allocated fence",
			mutate: func(t *testing.T, db *DB) {
				if _, err := db.db.Exec(`UPDATE meta SET value='1' WHERE key='fencing'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing counter",
			want: "read fencing counter",
			mutate: func(t *testing.T, db *DB) {
				if _, err := db.db.Exec(`DELETE FROM meta WHERE key='fencing'`); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := validRunStateDB(t)
			tc.mutate(t, db)
			if err := db.ValidateRunState(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRunState() = %v, want %q", err, tc.want)
			}
		})
	}
}
