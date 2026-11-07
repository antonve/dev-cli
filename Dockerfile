# Headless teardown image: a static `dev` and a pinned kubectl, run as a
# non-root user with no shell. See docs/architecture.md "Headless teardown".
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/dev ./cmd/dev

FROM registry.k8s.io/kubectl:v1.36.5@sha256:0667e3f3e6ef435d7cd4c16684244703ebd67314c17511c2a9f87ea44f6cdf6b AS kubectl

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=kubectl /bin/kubectl /usr/local/bin/kubectl
COPY --from=build /out/dev /usr/local/bin/dev
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/dev"]
