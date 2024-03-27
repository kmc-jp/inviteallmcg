FROM golang:1.22 as builder
ENV ROOT=/build
RUN mkdir ${ROOT}
WORKDIR ${ROOT}

COPY ./go.mod ./go.sum $ROOT/
RUN go get

COPY . $ROOT
RUN CGO_ENABLED=0 GOOS=linux go build -o main $ROOT/main.go && chmod +x ./main

FROM alpine:3
WORKDIR /app

COPY --from=builder /build/main ./
COPY --from=builder /usr/share/zoneinfo/Asia/Tokyo /usr/share/zoneinfo/Asia/Tokyo
CMD ["./main"]
