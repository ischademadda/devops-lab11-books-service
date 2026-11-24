# ---------- Stage 1: builder — компиляция статического бинарника ----------
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Сначала зависимости — этот слой кэшируется, пока go.mod/go.sum не меняются
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/books .

# ---------- Stage 2: runtime — пустой образ с одним бинарником ----------
FROM scratch AS runtime

COPY --from=builder /out/books /books

USER 10001:10001
EXPOSE 8000

# В scratch нет curl/shell, поэтому healthcheck — сам бинарник
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
    CMD ["/books", "-healthcheck"]

ENTRYPOINT ["/books"]
