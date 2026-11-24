# Books microservice (ЛР 11)

RESTful микросервис для учёта книг. Стек: **Go 1.23** (стандартный `net/http` + `pgx`), **PostgreSQL 16**, **Docker** (multi-stage) + **docker-compose**.

## Запуск

```bash
git clone <URL репозитория>
cd <папка репозитория>
docker compose up -d --build
```

Сервис: <http://localhost:8000>, Swagger UI: <http://localhost:8000/docs> (спецификация — `openapi.yaml`).
Параметры (необязательно) — через `.env` (см. `.env.example`).

Остановка: `docker compose down` (данные сохраняются в volume `pgdata`; `down -v` — удалить и данные).

## API

| Метод  | URL                  | Описание                                   | Успех | Ошибки        |
|--------|----------------------|--------------------------------------------|-------|---------------|
| GET    | `/health`            | Проверка сервиса и БД                      | 200   | 503           |
| GET    | `/books`             | Список (`?author=`, `?limit=`, `?offset=`) | 200   | 422           |
| GET    | `/books/{id}`        | Книга по id                                | 200   | 404, 422      |
| POST   | `/books`             | Создать книгу                              | 201   | 409, 422      |
| PATCH  | `/books/{id}`        | Частичное обновление                       | 200   | 404, 409, 422 |
| DELETE | `/books/{id}`        | Удалить                                    | 204   | 404           |

Пример тела `POST /books`:

```json
{"title": "Мастер и Маргарита", "author": "Михаил Булгаков", "year": 1967, "isbn": "978-5-17-090000-1"}
```

## Обработка ошибок

Сервис не падает на некорректных данных — всегда отвечает JSON `{"error": "...", "details": [...]}`:

- `404` — книга/маршрут не найдены; `409` — ISBN уже существует;
- `422` — невалидное тело/параметры (пустые поля, не тот тип, год вне диапазона, битый JSON, пустое тело, пустой PATCH);
- `405` — неподдерживаемый метод; `413` — слишком большое тело; `503` — проблемы с БД; `500` — непредвиденная ошибка (паника перехватывается).

Все запросы и ошибки логируются (`docker compose logs -f api`), уровень — переменная `LOG_LEVEL`.

## Multi-stage сборка

`Dockerfile`: стадия `builder` (`golang:alpine`) компилирует статический бинарник, стадия `runtime` — `FROM scratch`
с одним бинарником (~14 МБ), запуск от непривилегированного пользователя, `HEALTHCHECK` через сам бинарник (`/books -healthcheck`).
В compose API стартует после того, как БД стала `healthy`.

## Проверка в Postman

Импортируйте `postman/books-service.postman_collection.json`. Переменная `baseUrl` по умолчанию `http://localhost:8000`.
Коллекция содержит 18 запросов с тестами (успешные сценарии, все виды ошибок, конфликт ISBN) и может запускаться повторно.

Из консоли: `docker run --rm --network host -v $PWD/postman:/etc/newman:ro postman/newman run books-service.postman_collection.json`
