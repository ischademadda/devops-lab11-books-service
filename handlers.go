package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type API struct {
	store *Store
}

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

var isbnRe = regexp.MustCompile(`^[0-9][0-9-]{8,15}[0-9Xx]$`)

const msgInvalid = "Некорректные данные запроса"

// ---------- ответы ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, details ...FieldError) {
	if rec, ok := w.(*statusRecorder); ok {
		rec.errMsg = msg
	}
	body := map[string]any{"error": msg}
	if len(details) > 0 {
		body["details"] = details
	}
	writeJSON(w, status, body)
}

// storeError переводит ошибки слоя БД в HTTP-ответы.
func storeError(w http.ResponseWriter, r *http.Request, err error, isbn string) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "Книга не найдена")
	case errors.Is(err, ErrDuplicateISBN):
		writeError(w, http.StatusConflict, fmt.Sprintf("Книга с ISBN %s уже существует", isbn))
	default:
		slog.Error("database error", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "База данных недоступна или вернула ошибку")
	}
}

// methods раздаёт запрос по HTTP-методу; на остальные отвечает JSON-ом 405.
func methods(m map[string]http.HandlerFunc) http.HandlerFunc {
	allowed := make([]string, 0, len(m))
	for k := range m {
		allowed = append(allowed, k)
	}
	sort.Strings(allowed)
	allow := strings.Join(allowed, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		if h, ok := m[r.Method]; ok {
			h(w, r)
			return
		}
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "Not Found")
}

// ---------- разбор и валидация ----------

// parseID возвращает id из пути. ok=false — ответ уже отправлен.
func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, msgInvalid,
			FieldError{"path.book_id", "Должно быть целым числом"})
		return 0, false
	}
	if n < 1 || n > math.MaxInt32 { // вне диапазона SERIAL — такой книги быть не может
		writeError(w, http.StatusNotFound, fmt.Sprintf("Книга с id=%d не найдена", n))
		return 0, false
	}
	return n, true
}

// decodeObject читает тело запроса как JSON-объект. status != 0 — ошибка.
func decodeObject(w http.ResponseWriter, r *http.Request) (obj map[string]any, status int, msg string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			return nil, http.StatusUnprocessableEntity, "Тело запроса пустое"
		case errors.As(err, &tooBig):
			return nil, http.StatusRequestEntityTooLarge, "Тело запроса слишком большое"
		default:
			return nil, http.StatusUnprocessableEntity, "Невалидный JSON"
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) { // мусор после объекта
		return nil, http.StatusUnprocessableEntity, "Невалидный JSON"
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, http.StatusUnprocessableEntity, "Ожидается JSON-объект"
	}
	return m, 0, ""
}

// validateBook проверяет поля книги. create=true — title/author/year обязательны.
// Возвращает только переданные и валидные поля: string / int / nil (для isbn).
func validateBook(m map[string]any, create bool) (map[string]any, []FieldError) {
	out := map[string]any{}
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{"body." + field, msg}) }

	for _, f := range []struct {
		name string
		max  int
	}{{"title", 200}, {"author", 100}} {
		v, present := m[f.name]
		if !present {
			if create {
				add(f.name, "Обязательное поле")
			}
			continue
		}
		s, isStr := v.(string)
		switch n := utf8.RuneCountInString(s); {
		case !isStr:
			add(f.name, "Должно быть строкой (не null)")
		case n < 1:
			add(f.name, "Не может быть пустой строкой")
		case n > f.max:
			add(f.name, fmt.Sprintf("Максимум %d символов", f.max))
		default:
			out[f.name] = s
		}
	}

	if v, present := m["year"]; !present {
		if create {
			add("year", "Обязательное поле")
		}
	} else {
		num, isNum := v.(json.Number)
		year, err := int64(0), error(nil)
		if isNum {
			year, err = num.Int64()
		}
		switch {
		case !isNum || err != nil:
			add("year", "Должно быть целым числом")
		case year < 1 || year > 2100:
			add("year", "Должно быть в диапазоне 1..2100")
		default:
			out["year"] = int(year)
		}
	}

	if v, present := m["isbn"]; present {
		switch s := v.(type) {
		case nil:
			out["isbn"] = nil
		case string:
			if isbnRe.MatchString(s) {
				out["isbn"] = s
			} else {
				add("isbn", "Ожидается ISBN-10/ISBN-13 (цифры и дефисы)")
			}
		default:
			add("isbn", "Должно быть строкой или null")
		}
	}
	return out, errs
}

func isbnOf(vals map[string]any) (isbn *string, text string) {
	if s, ok := vals["isbn"].(string); ok {
		return &s, s
	}
	return nil, ""
}

// ---------- эндпоинты ----------

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.Ping(ctx); err != nil {
		storeError(w, r, err, "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) listBooks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var errs []FieldError
	intParam := func(name string, def, min, max int) int {
		s := q.Get(name)
		if s == "" {
			return def
		}
		n, err := strconv.Atoi(s)
		switch {
		case err != nil:
			errs = append(errs, FieldError{"query." + name, "Должно быть целым числом"})
		case n < min:
			errs = append(errs, FieldError{"query." + name, fmt.Sprintf("Должно быть >= %d", min)})
		case n > max:
			errs = append(errs, FieldError{"query." + name, fmt.Sprintf("Должно быть <= %d", max)})
		default:
			return n
		}
		return def
	}
	limit := intParam("limit", 50, 1, 200)
	offset := intParam("offset", 0, 0, math.MaxInt32)
	if len(errs) > 0 {
		writeError(w, http.StatusUnprocessableEntity, msgInvalid, errs...)
		return
	}
	books, err := a.store.List(r.Context(), q.Get("author"), limit, offset)
	if err != nil {
		storeError(w, r, err, "")
		return
	}
	writeJSON(w, http.StatusOK, books)
}

func (a *API) getBook(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	b, err := a.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Книга с id=%d не найдена", id))
		return
	}
	if err != nil {
		storeError(w, r, err, "")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (a *API) createBook(w http.ResponseWriter, r *http.Request) {
	m, status, msg := decodeObject(w, r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	vals, errs := validateBook(m, true)
	if len(errs) > 0 {
		writeError(w, http.StatusUnprocessableEntity, msgInvalid, errs...)
		return
	}
	isbn, isbnText := isbnOf(vals)
	b, err := a.store.Create(r.Context(), vals["title"].(string), vals["author"].(string), vals["year"].(int), isbn)
	if err != nil {
		storeError(w, r, err, isbnText)
		return
	}
	slog.Info("book created", "id", b.ID, "title", b.Title)
	writeJSON(w, http.StatusCreated, b)
}

func (a *API) updateBook(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	m, status, msg := decodeObject(w, r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	vals, errs := validateBook(m, false)
	if len(errs) > 0 {
		writeError(w, http.StatusUnprocessableEntity, msgInvalid, errs...)
		return
	}
	if len(vals) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "Не передано ни одного поля для обновления")
		return
	}
	_, isbnText := isbnOf(vals)
	b, err := a.store.Update(r.Context(), id, vals)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Книга с id=%d не найдена", id))
		return
	}
	if err != nil {
		storeError(w, r, err, isbnText)
		return
	}
	slog.Info("book updated", "id", b.ID)
	writeJSON(w, http.StatusOK, b)
}

func (a *API) deleteBook(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	err := a.store.Delete(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Книга с id=%d не найдена", id))
		return
	}
	if err != nil {
		storeError(w, r, err, "")
		return
	}
	slog.Info("book deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}
