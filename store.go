package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Book struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Year      int       `json:"year"`
	ISBN      *string   `json:"isbn"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	ErrNotFound      = errors.New("book not found")
	ErrDuplicateISBN = errors.New("duplicate isbn")
)

const bookColumns = "id, title, author, year, isbn, created_at"

type Store struct {
	pool *pgxpool.Pool
}

// NewStore подключается к БД (с повторными попытками, пока СУБД поднимается) и создаёт таблицы.
func NewStore(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	const attempts = 30
	for i := 1; i <= attempts; i++ {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		slog.Warn("database not ready", "attempt", i, "of", attempts, "err", err)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("database is not available: %w", err)
	}
	slog.Info("database is ready")

	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS books (
			id         SERIAL PRIMARY KEY,
			title      VARCHAR(200) NOT NULL,
			author     VARCHAR(100) NOT NULL,
			year       INTEGER      NOT NULL,
			isbn       VARCHAR(17)  UNIQUE,
			created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS ix_books_author ON books (author)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			pool.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	slog.Info("tables created / verified")
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func scanBook(row pgx.Row) (Book, error) {
	var b Book
	err := row.Scan(&b.ID, &b.Title, &b.Author, &b.Year, &b.ISBN, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, mapErr(err)
}

// mapErr превращает нарушение уникальности ISBN в доменную ошибку.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicateISBN
	}
	return err
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Store) List(ctx context.Context, author string, limit, offset int) ([]Book, error) {
	q := "SELECT " + bookColumns + " FROM books"
	args := []any{}
	if author != "" {
		args = append(args, "%"+escapeLike(author)+"%")
		q += " WHERE author ILIKE $1"
	}
	q += fmt.Sprintf(" ORDER BY id LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	books := []Book{} // не nil, чтобы в JSON был [] а не null
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

func (s *Store) Get(ctx context.Context, id int64) (Book, error) {
	return scanBook(s.pool.QueryRow(ctx, "SELECT "+bookColumns+" FROM books WHERE id = $1", id))
}

func (s *Store) Create(ctx context.Context, title, author string, year int, isbn *string) (Book, error) {
	return scanBook(s.pool.QueryRow(ctx,
		"INSERT INTO books (title, author, year, isbn) VALUES ($1, $2, $3, $4) RETURNING "+bookColumns,
		title, author, year, isbn))
}

// Update применяет только переданные поля (значение nil для isbn сбрасывает его в NULL).
func (s *Store) Update(ctx context.Context, id int64, changes map[string]any) (Book, error) {
	sets := []string{}
	args := []any{}
	for _, col := range []string{"title", "author", "year", "isbn"} { // белый список колонок
		if v, ok := changes[col]; ok {
			args = append(args, v)
			sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	args = append(args, id)
	q := fmt.Sprintf("UPDATE books SET %s WHERE id = $%d RETURNING %s",
		strings.Join(sets, ", "), len(args), bookColumns)
	return scanBook(s.pool.QueryRow(ctx, q, args...))
}

func (s *Store) Delete(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM books WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
