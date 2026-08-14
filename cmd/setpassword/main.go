// Command setpassword sets an account's password directly in the database.
//
// It exists for the accounts that cannot be created through /auth/register:
// the seed account, whose password_hash was written as a deliberate invalid
// placeholder, and the demo account from 000016. Both exist before anything
// can log in as them, so there is no authenticated path to a password change.
//
// The password is read from stdin rather than a flag, because a flag is
// visible in the process table and in shell history:
//
//	read -rs -p 'password: ' PW && printf '%s' "$PW" | go run ./cmd/setpassword -email you@example.com
//
// -hash-only prints a bcrypt hash and touches nothing, for when the update has
// to happen through psql against a database this machine cannot reach.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"

	"github.com/EngenMe/applymind-backend/internal/auth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	email := flag.String("email", "", "email address of the account to update")
	hashOnly := flag.Bool("hash-only", false, "print a bcrypt hash and exit without touching the database")
	flag.Parse()

	// Normalised the same way the service does before its uniqueness check, so
	// this command and a later login agree on which row they mean. The column
	// is citext, so this is belt and braces — which is the point.
	normalised := strings.ToLower(strings.TrimSpace(*email))
	if normalised == "" && !*hashOnly {
		return errors.New("-email is required (or use -hash-only)")
	}

	password, err := readPassword(os.Stdin)
	if err != nil {
		return err
	}
	// The same minimum the service enforces, referenced rather than repeated so
	// the two cannot drift apart.
	if len([]rune(password)) < auth.MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", auth.MinPasswordLength)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), auth.BcryptCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	if *hashOnly {
		fmt.Println(string(hash))
		return nil
	}

	// Local convenience only. Under any real deployment the variable is already
	// in the environment and a missing .env is not an error.
	_ = godotenv.Load()
	dsn := firstNonEmpty(os.Getenv("NEON_DATABASE_URL"), os.Getenv("DATABASE_URL"))
	if dsn == "" {
		return errors.New("NEON_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	tag, err := conn.Exec(
		ctx,
		`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE email = $2`,
		string(hash), normalised,
	)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no account with email %q", normalised)
	}

	// Existing sessions are deliberately left alone. This command writes a
	// password where there effectively was none; it is not a reset after a
	// compromise. If it ever becomes that, revoking every session for the user
	// belongs here and the omission would be the bug.
	fmt.Printf("password set for %s\n", normalised)
	return nil
}

// readPassword takes the whole of stdin as the password.
func readPassword(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	if len(data) == 0 {
		return "", errors.New("no password on stdin")
	}
	// Only the line ending is stripped. Leading and trailing spaces are part of
	// the password — the service does not trim it either, so trimming here
	// would set a password that then cannot be used to log in.
	return strings.TrimRight(string(data), "\r\n"), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
