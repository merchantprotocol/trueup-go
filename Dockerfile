# Test image: runs the SDK's tests (live tests need TRUEUP_API_KEY).
FROM golang:1.23
WORKDIR /sdk
COPY . .
RUN go vet ./...
CMD ["go", "test", "-v", "-count=1", "./..."]
