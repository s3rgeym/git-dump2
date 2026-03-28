# git-dump

Инструмент для скачивания и восстановления открытых `.git`-директорий с веб-серверов.

## Использование

```
git-dump [флаги]
```

URL-адреса передаются через stdin или файл (`-i`):

```sh
cat urls.txt | git-dump -w 20 -rps 100 -d ./output
git-dump -i urls.txt -w 20 -rps 100 -d ./output
```

### Флаги

| Флаг        | По умолчанию | Описание                            |
|-------------|--------------|-------------------------------------|
| `-i`        | `-` (stdin)  | Файл со списком URL                 |
| `-o`        | —            | Файл для вывода результатов (JSONL) |
| `-d`        | `dumps`      | Директория для сохранения дампов    |
| `-ua`       | —            | User-Agent (случайный если не задан)|
| `-w`        | `10`         | Количество воркеров                 |
| `-rps`      | `50`         | Лимит запросов в секунду            |
| `-maxerr`   | `30`         | Максимум ошибок на хост             |
| `-maxretry` | `1`          | Количество повторов при ошибке      |
| `-f`        | `false`      | Перезаписывать существующие файлы   |
| `-ct`       | `5s`         | Таймаут подключения                 |
| `-rt`       | `15s`        | Таймаут запроса                     |
| `-version`  | —            | Вывести версию и выйти              |

## Сборка

### Локальная сборка

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o git-dump .
```

- **`CGO_ENABLED=0`** — статическая сборка без зависимости от системных C-библиотек. Бинарь запускается в минимальных окружениях: Alpine, scratch-контейнеры, любые Linux-дистрибутивы.
- **`-trimpath`** — убирает абсолютные пути к исходникам из бинаря. Сборка не раскрывает локальную структуру директорий разработчика и является воспроизводимой.
- **`-ldflags="-s -w"`** — убирает таблицу символов и отладочную информацию DWARF, уменьшает размер бинаря.

### Сборка с версией из тега

```sh
VERSION=$(git describe --tags --always --dirty)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o git-dump .
```

Версию можно проверить:

```sh
./git-dump -version
# v1.2.3
```

### Кросс-компиляция

```sh
VERSION=$(git describe --tags --always)
LDFLAGS="-s -w -X main.version=${VERSION}"

GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o dist/git-dump-linux-amd64   .
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o dist/git-dump-linux-arm64   .
GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o dist/git-dump-darwin-amd64  .
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o dist/git-dump-darwin-arm64  .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o dist/git-dump-windows-amd64.exe .
```

## Релиз

Для создания релиза достаточно поставить тег и запушить его:

```sh
git tag v1.0.0
git push origin v1.0.0
```

GitHub Actions автоматически соберёт бинари для всех платформ и создаст релиз с артефактами.
