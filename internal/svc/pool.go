package svc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const acquireTimeout = time.Second

// Pool is the database pool as the service sees it. Hold(n) keeps n
// connections checked out and is how the pool-leak fault starves requests.
type Pool interface {
	Query(ctx context.Context) error
	Ping(ctx context.Context) error
	Hold(n int) error
	Held() int
}

type pgPool struct {
	pool *pgxpool.Pool
	size int

	mu   sync.Mutex
	held []*pgxpool.Conn
}

// NewPGPool connects lazily, so it succeeds before Postgres is ready and
// requests fail with 500 until it is.
func NewPGPool(ctx context.Context, url string, size int32) (Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	cfg.MaxConns = size
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return &pgPool{pool: pool, size: int(size)}, nil
}

func (p *pgPool) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()
	return p.pool.Acquire(ctx)
}

func (p *pgPool) Query(ctx context.Context) error {
	conn, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}

func (p *pgPool) Ping(ctx context.Context) error {
	conn, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return conn.Ping(ctx)
}

func (p *pgPool) Hold(n int) error {
	if n < 0 || n > p.size {
		return fmt.Errorf("hold %d: pool size is %d", n, p.size)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.held) < n {
		conn, err := p.acquire(context.Background())
		if err != nil {
			return fmt.Errorf("hold %d: holding %d: %w", n, len(p.held), err)
		}
		p.held = append(p.held, conn)
	}
	for len(p.held) > n {
		last := len(p.held) - 1
		p.held[last].Release()
		p.held = p.held[:last]
	}
	return nil
}

func (p *pgPool) Held() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.held)
}
