package poets

import (
	"fmt"

	"github.com/spacemeshos/go-spacemesh/sql"
)

// Get gets a PoET for a given ref.
func Get(db sql.Executor, ref []byte) (poet []byte, err error) {
	enc := func(stmt *sql.Statement) {
		stmt.BindBytes(1, ref)
	}
	dec := func(stmt *sql.Statement) bool {
		poet = make([]byte, stmt.ColumnLen(0))
		stmt.ColumnBytes(0, poet[:])
		return true
	}

	rows, err := db.Exec("select poet from poets where ref = ?1;", enc, dec)
	if err != nil {
		return nil, fmt.Errorf("get value: %w", err)
	}
	if rows == 0 {
		return nil, fmt.Errorf("get value: %w", sql.ErrNotFound)
	}

	return poet, nil
}

// Add adds a poet for a given ref.
func Add(db sql.Executor, ref, poet, serviceID []byte, roundID string) error {
	enc := func(stmt *sql.Statement) {
		stmt.BindBytes(1, ref)
		stmt.BindBytes(2, poet)
		stmt.BindBytes(3, serviceID)
		stmt.BindBytes(4, []byte(roundID))
	}
	_, err := db.Exec(`
		insert into poets (ref, poet, service_id, round_id) 
		values (?1, ?2, ?3, ?4);`, enc, nil)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	return nil
}

// GetRef gets a PoET ref for a given service ID and round ID.
func GetRef(db sql.Executor, poetID []byte, roundID string) (ref []byte, err error) {
	enc := func(stmt *sql.Statement) {
		stmt.BindBytes(1, poetID)
		stmt.BindBytes(2, []byte(roundID))
	}
	dec := func(stmt *sql.Statement) bool {
		ref = make([]byte, stmt.ColumnLen(0))
		stmt.ColumnBytes(0, ref[:])
		return true
	}

	rows, err := db.Exec(`
		select ref from poets 
		where service_id = ?1 and round_id = ?2;`, enc, dec)
	if err != nil {
		return nil, fmt.Errorf("get value: %w", err)
	}
	if rows == 0 {
		return nil, fmt.Errorf("get value: %w", sql.ErrNotFound)
	}

	return ref, nil
}
