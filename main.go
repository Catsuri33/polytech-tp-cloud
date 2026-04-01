package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

var appName string
var connectionString string
var databaseConn *pgx.Conn
var redisClient *redis.Client
var clientsMu sync.Mutex
var clients = make(map[chan string]bool)

type Todo struct {
	ID          uint32     `json:"id"`
	Title       *string    `json:"title"`
	Description *string    `json:"description"`
	Due_Date    *time.Time `json:"due_date"`
	Status      *string    `json:"status"`
	Created_At  *time.Time `json:"created_at"`
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func Connect(connectionString string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(context.Background(), connectionString)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// -------------------------------------------------------------------
// Logging middleware
// -------------------------------------------------------------------

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, wrapped.status, time.Since(start).Round(time.Millisecond))
	})
}

// -------------------------------------------------------------------
// Handlers
// -------------------------------------------------------------------

func getHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		databaseRes := "disconnected"
		if databaseConn != nil {
			databaseRes = "connected"
		}
		jsonResponse(w, http.StatusOK, map[string]string{
			"status":   "ok",
			"app":      appName,
			"database": databaseRes,
		})
	}
}

func getTodos() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")

		var rows pgx.Rows
		var err error

		if status != "" {
			rows, err = databaseConn.Query(context.Background(), "SELECT * FROM todos WHERE status = $1", status)
		} else {
			rows, err = databaseConn.Query(context.Background(), "SELECT * FROM todos")
		}

		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()

		var todos []Todo
		for rows.Next() {
			var todo Todo
			if err := rows.Scan(&todo.ID, &todo.Title, &todo.Description, &todo.Due_Date, &todo.Status, &todo.Created_At); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			todos = append(todos, todo)
		}

		if todos == nil {
			todos = []Todo{}
		}
		jsonResponse(w, http.StatusOK, todos)
	}
}

func createTodo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var todo Todo
		if err := json.NewDecoder(r.Body).Decode(&todo); err != nil || todo.Title == nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "title est obligatoire"})
			return
		}

		var id uint32
		err := databaseConn.QueryRow(
			context.Background(),
			`INSERT INTO todos (title, description, due_date, status)
			 VALUES ($1, $2, $3, $4) RETURNING id`,
			todo.Title, todo.Description, todo.Due_Date, todo.Status,
		).Scan(&id)

		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		todo.ID = id
		jsonResponse(w, http.StatusCreated, todo)
	}
}

func editTodo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
			return
		}

		var todo Todo
		if err := json.NewDecoder(r.Body).Decode(&todo); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		cmdTag, err := databaseConn.Exec(
			context.Background(),
			`UPDATE todos
			 SET title = COALESCE($1, title),
			     description = COALESCE($2, description),
			     due_date = COALESCE($3, due_date),
			     status = COALESCE($4, status)
			 WHERE id = $5`,
			todo.Title, todo.Description, todo.Due_Date, todo.Status, id,
		)

		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if cmdTag.RowsAffected() == 0 {
			jsonResponse(w, http.StatusNotFound, map[string]string{"error": "todo not found"})
			return
		}

		jsonResponse(w, http.StatusOK, map[string]string{"message": "todo updated"})
	}
}

func deleteTodo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
			return
		}

		cmdTag, err := databaseConn.Exec(context.Background(), "DELETE FROM todos WHERE id = $1", id)
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if cmdTag.RowsAffected() == 0 {
			jsonResponse(w, http.StatusNotFound, map[string]string{"error": "todo not found"})
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func getOverdueTodos() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := databaseConn.Query(
			context.Background(),
			"SELECT * FROM todos WHERE due_date < NOW() AND status != 'done'",
		)
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()

		var todos []Todo
		for rows.Next() {
			var todo Todo
			if err := rows.Scan(&todo.ID, &todo.Title, &todo.Description, &todo.Due_Date, &todo.Status, &todo.Created_At); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			todos = append(todos, todo)
		}

		if todos == nil {
			todos = []Todo{}
		}
		jsonResponse(w, http.StatusOK, todos)
	}
}

func alerts() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
			return
		}

		clientChan := make(chan string)

		clientsMu.Lock()
		clients[clientChan] = true
		clientsMu.Unlock()

		defer func() {
			clientsMu.Lock()
			delete(clients, clientChan)
			clientsMu.Unlock()
			close(clientChan)
		}()

		pingTicker := time.NewTicker(30 * time.Second)
		defer pingTicker.Stop()

		clientGone := r.Context().Done()

		for {
			select {
			case <-clientGone:
				return
			case msg := <-clientChan:
				fmt.Fprintf(w, "event: todo_alert\n")
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			case <-pingTicker.C:
				fmt.Fprintf(w, "event: ping\n")
				fmt.Fprintf(w, "data: {}\n\n")
				flusher.Flush()
			}
		}
	}
}

func notify() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
			return
		}

		var todo Todo
		err = databaseConn.QueryRow(
			context.Background(),
			`SELECT id, title, description, due_date, status, created_at FROM todos WHERE id=$1`,
			id,
		).Scan(&todo.ID, &todo.Title, &todo.Description, &todo.Due_Date, &todo.Status, &todo.Created_At)

		if err != nil {
			jsonResponse(w, http.StatusNotFound, map[string]string{"error": "todo not found"})
			return
		}

		payload := fmt.Sprintf(
			`{"id": %d, "title": "%s", "status": "%s", "due_date": "%s"}`,
			todo.ID,
			func() string {
				if todo.Title == nil {
					return ""
				}
				return *todo.Title
			}(),
			func() string {
				if todo.Status == nil {
					return ""
				}
				return *todo.Status
			}(),
			func() string {
				if todo.Due_Date == nil {
					return ""
				}
				return todo.Due_Date.Format("2006-01-02")
			}(),
		)

		redisClient.Publish(context.Background(), "todo_alerts", payload)

		clientsMu.Lock()
		nbClients := len(clients)
		clientsMu.Unlock()

		jsonResponse(w, http.StatusOK, map[string]any{
			"message":   "Alerte envoyée",
			"listeners": nbClients,
		})
	}
}

// -------------------------------------------------------------------
// Main
// -------------------------------------------------------------------

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	connectionString = os.Getenv("POSTGRESQL_ADDON_URI")
	appName = os.Getenv("APP_NAME")
	if appName == "" {
		appName = "My awesome API"
	}
	log.Printf("App: %s | Port: %s", appName, port)

	// Connect to the database
	log.Println("Connecting to database...")
	var err error
	databaseConn, err = Connect(connectionString)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer databaseConn.Close(context.Background())
	log.Println("Database connected")

	// Migrations
	log.Println("Running migrations...")
	_, err = databaseConn.Exec(context.Background(), `
		DO $$ BEGIN
			CREATE TYPE status AS ENUM ('pending', 'done');
		EXCEPTION
			WHEN duplicate_object THEN NULL;
		END $$;
	`)
	if err != nil {
		log.Fatalf("Failed to create type: %v", err)
	}

	_, err = databaseConn.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS todos (
			id          SERIAL PRIMARY KEY,
			title       VARCHAR(60) NOT NULL,
			description VARCHAR(255) NULL,
			due_date    DATE NULL,
			status      status NOT NULL DEFAULT 'pending',
			created_at  TIMESTAMP NOT NULL DEFAULT NOW()
		);`)
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}
	log.Println("Migrations done")

	// Connect Redis
	log.Println("Connecting to Redis...")
	opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		log.Fatalf("Failed to parse Redis URL: %v", err)
	}
	redisClient = redis.NewClient(opt)
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Println("Redis connected")

	// Goroutine for alerts
	go func() {
		log.Println("Subscribing to Redis channel todo_alerts...")
		sub := redisClient.Subscribe(context.Background(), "todo_alerts")
		ch := sub.Channel()
		log.Println("Subscribed to Redis channel todo_alerts")
		for msg := range ch {
			clientsMu.Lock()
			log.Printf("Broadcasting alert to %d clients", len(clients))
			for client := range clients {
				client <- msg.Payload
			}
			clientsMu.Unlock()
		}
	}()

	// Router
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", getHealth())
	mux.HandleFunc("GET /todos", getTodos())
	mux.HandleFunc("POST /todos", createTodo())
	mux.HandleFunc("PATCH /todos/{id}", editTodo())
	mux.HandleFunc("DELETE /todos/{id}", deleteTodo())
	mux.HandleFunc("GET /todos/overdue", getOverdueTodos())
	mux.HandleFunc("GET /alerts", alerts())
	mux.HandleFunc("POST /todos/{id}/notify", notify())

	log.Printf("Starting server on 0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, loggingMiddleware(mux)))
}
