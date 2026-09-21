FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN for c in router fakebackend loadgen idprobe; do \
      CGO_ENABLED=0 go build -o /out/$c ./cmd/$c; done

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/ /usr/local/bin/
USER nonroot
