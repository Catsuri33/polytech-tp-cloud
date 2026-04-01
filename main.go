package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

var (
	appName string
	db      *sql.DB

	redisClient *redis.Client

	clientsMu sync.Mutex
	clients   = make(map[chan string]bool)
)

// ----------------------
// Models
// ----------------------

type Todo struct {
	ID          uint32     `json:"id"`
	Title       *string    `json:"title"`
	Description *string    `json:"description"`
	DueDate     *time.Time `json:"due_date"`
	Status      *string    `json:"status"`
	CreatedAt   *time.Time `json:"created_at"`
}

// ----------------------
// Helpers
// ----------------------

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ----------------------
// Handlers
// ----------------------

func getHealth(w http.ResponseWriter, r *http.Request) {
	dbStatus := "disconnected"
	if db != nil {
		dbStatus = "connected"
	}

	jsonResponse(w, 200, map[string]string{
		"status":   "ok",
		"app":      appName,
		"database": dbStatus,
	})
}

func getTodos(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	var (
		rows *sql.Rows
		err  error
	)

	if status != "" {
		rows, err = db.Query(`SELECT id, title, description, due_date, status, created_at FROM todos WHERE status=$1`, status)
	} else {
		rows, err = db.Query(`SELECT id, title, description, due_date, status, created_at FROM todos`)
	}

	if err != nil {
		jsonResponse(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var todos []Todo
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.DueDate, &t.Status, &t.CreatedAt); err != nil {
			jsonResponse(w, 500, map[string]string{"error": err.Error()})
			return
		}
		todos = append(todos, t)
	}

	if todos == nil {
		todos = []Todo{}
	}

	jsonResponse(w, 200, todos)
}

func createTodo(w http.ResponseWriter, r *http.Request) {
	var t Todo
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil || t.Title == nil {
		jsonResponse(w, 400, map[string]string{"error": "title required"})
		return
	}

	var id int
	err := db.QueryRow(
		`INSERT INTO todos (title, description, due_date, status)
		 VALUES ($1,$2,$3,$4) RETURNING id`,
		t.Title, t.Description, t.DueDate, t.Status,
	).Scan(&id)

	if err != nil {
		jsonResponse(w, 500, map[string]string{"error": err.Error()})
		return
	}

	t.ID = uint32(id)
	jsonResponse(w, 201, t)
}

func editTodo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonResponse(w, 400, map[string]string{"error": "invalid id"})
		return
	}

	var t Todo
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		jsonResponse(w, 400, map[string]string{"error": err.Error()})
		return
	}

	res, err := db.Exec(`
		UPDATE todos
		SET title = COALESCE($1, title),
			description = COALESCE($2, description),
			due_date = COALESCE($3, due_date),
			status = COALESCE($4, status)
		WHERE id=$5
	`, t.Title, t.Description, t.DueDate, t.Status, id)

	if err != nil {
		jsonResponse(w, 500, map[string]string{"error": err.Error()})
		return
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		jsonResponse(w, 404, map[string]string{"error": "todo not found"})
		return
	}

	jsonResponse(w, 200, map[string]string{"message": "updated"})
}

func deleteTodo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonResponse(w, 400, map[string]string{"error": "invalid id"})
		return
	}

	res, err := db.Exec(`DELETE FROM todos WHERE id=$1`, id)
	if err != nil {
		jsonResponse(w, 500, map[string]string{"error": err.Error()})
		return
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		jsonResponse(w, 404, map[string]string{"error": "not found"})
		return
	}

	w.WriteHeader(204)
}

func getOverdueTodos(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT id, title, description, due_date, status, created_at
		FROM todos
		WHERE due_date < NOW() AND status != 'done'
	`)
	if err != nil {
		jsonResponse(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var todos []Todo
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.DueDate, &t.Status, &t.CreatedAt); err != nil {
			jsonResponse(w, 500, map[string]string{"error": err.Error()})
			return
		}
		todos = append(todos, t)
	}

	if todos == nil {
		todos = []Todo{}
	}

	jsonResponse(w, 200, todos)
}

// ----------------------
// SSE alerts
// ----------------------

func alerts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, 500, map[string]string{"error": "stream not supported"})
		return
	}

	ch := make(chan string)

	clientsMu.Lock()
	clients[ch] = true
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, ch)
		clientsMu.Unlock()
		close(ch)
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: todo_alert\n")
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, "event: ping\n")
			fmt.Fprintf(w, "data: {}\n\n")
			flusher.Flush()
		}
	}
}

func notify(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonResponse(w, 400, map[string]string{"error": "invalid id"})
		return
	}

	var t Todo
	err = db.QueryRow(`
		SELECT id, title, description, due_date, status, created_at
		FROM todos WHERE id=$1
	`, id).Scan(&t.ID, &t.Title, &t.Description, &t.DueDate, &t.Status, &t.CreatedAt)

	if err != nil {
		jsonResponse(w, 404, map[string]string{"error": "not found"})
		return
	}

	payload := fmt.Sprintf(`{"id":%d,"title":"%s","status":"%s"}`, t.ID, deref(t.Title), deref(t.Status))

	redisClient.Publish(context.Background(), "todo_alerts", payload)

	clientsMu.Lock()
	nb := len(clients)
	clientsMu.Unlock()

	jsonResponse(w, 200, map[string]any{
		"message":   "sent",
		"listeners": nb,
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ----------------------
// main
// ----------------------

func main() {
	_ = godotenv.Load()

	appName = os.Getenv("APP_NAME")
	if appName == "" {
		appName = "Todo API"
	}

	dsn := os.Getenv("DATABASE_URL")

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}

	if err := db.Ping(); err != nil {
		log.Fatal(err)
	}

	redisOpt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		log.Fatal(err)
	}

	redisClient = redis.NewClient(redisOpt)
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		log.Fatal(err)
	}

	// Redis subscriber -> broadcast to SSE clients
	go func() {
		sub := redisClient.Subscribe(context.Background(), "todo_alerts")
		ch := sub.Channel()

		for msg := range ch {
			clientsMu.Lock()
			for c := range clients {
				select {
				case c <- msg.Payload:
				default:
				}
			}
			clientsMu.Unlock()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", getHealth)
	mux.HandleFunc("GET /todos", getTodos)
	mux.HandleFunc("POST /todos", createTodo)
	mux.HandleFunc("PATCH /todos/{id}", editTodo)
	mux.HandleFunc("DELETE /todos/{id}", deleteTodo)
	mux.HandleFunc("GET /todos/overdue", getOverdueTodos)
	mux.HandleFunc("GET /alerts", alerts)
	mux.HandleFunc("POST /todos/{id}/notify", notify)

	log.Println("Server running on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
