package grpc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

func TestConnectionPool_SerializesSlowConnectionCreationPerService(t *testing.T) {
	pool := NewConnectionPool(10, 2, time.Minute, true, nil)
	defer func() { _ = pool.CloseAll() }()

	var creates int32
	createFunc := func() (*grpc.ClientConn, error) {
		atomic.AddInt32(&creates, 1)
		time.Sleep(20 * time.Millisecond)
		return grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	const callers = 16
	var wg sync.WaitGroup
	errCh := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pool.GetConnection("svc", createFunc)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("GetConnection failed: %v", err)
		}
	}

	if got := atomic.LoadInt32(&creates); got != 1 {
		t.Fatalf("expected one slow create for concurrent callers, got %d", got)
	}
}

func TestConnectionPool_CloseConnectionWaitsForInFlightCreate(t *testing.T) {
	pool := NewConnectionPool(10, 2, time.Minute, true, nil)
	defer func() { _ = pool.CloseAll() }()

	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	createFunc := func() (*grpc.ClientConn, error) {
		close(createStarted)
		<-releaseCreate
		return grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	connCh := make(chan *grpc.ClientConn, 1)
	errCh := make(chan error, 2)
	go func() {
		conn, err := pool.GetConnection("svc", createFunc)
		if err != nil {
			errCh <- err
			return
		}
		connCh <- conn
	}()

	<-createStarted
	closeDone := make(chan struct{})
	go func() {
		errCh <- pool.CloseConnection("svc")
		close(closeDone)
	}()

	select {
	case <-closeDone:
		t.Fatal("CloseConnection returned before in-flight create completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseCreate)
	var conn *grpc.ClientConn
	select {
	case conn = <-connCh:
	case err := <-errCh:
		t.Fatalf("GetConnection failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("GetConnection did not return")
	}

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("CloseConnection did not return")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("CloseConnection failed: %v", err)
		}
	default:
	}

	if got := pool.TotalConnectionCount(); got != 0 {
		t.Fatalf("expected closed service to be removed from pool, got %d connections", got)
	}
	if state := conn.GetState(); state != connectivity.Shutdown {
		t.Fatalf("expected returned connection to be closed after CloseConnection, got %s", state)
	}
}
