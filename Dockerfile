FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod .
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /veyra-hub .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /veyra-hub /veyra-hub
VOLUME ["/data"]
EXPOSE 8787
USER nonroot:nonroot
ENTRYPOINT ["/veyra-hub", "-data", "/data"]
