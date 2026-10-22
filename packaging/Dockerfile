FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY . .
RUN test "$(go env GOVERSION)" = go1.27.2 \
 && go build -mod=readonly -trimpath -buildvcs=false -tags timetzdata -o /out/homed ./cmd/homed \
 && go build -mod=readonly -trimpath -buildvcs=false -tags timetzdata -o /out/homeward ./cmd/homeward \
 && go build -mod=readonly -trimpath -buildvcs=false -tags timetzdata -o /out/homeward-package ./cmd/homeward-package
RUN install -d -m 0700 -o 10001 -g 10001 /seed/var/lib/homeward /seed/run/homeward-credentials /seed/existing-postgres

FROM python:3.13.17-slim-bookworm@sha256:ac411330d543edd1cc55be253a783cff743082feddfa07957a4574ad1ea01a6b AS runtime
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
COPY packaging/requirements.lock /opt/homeward/requirements.lock
RUN python -m pip install --no-cache-dir --only-binary=:all: --require-hashes --no-deps -r /opt/homeward/requirements.lock \
 && python -m pip check
COPY drivers/caseta /opt/homeward/drivers/caseta
COPY testworker/generated /opt/homeward/testworker/generated
COPY testworker/schema_digest.py /opt/homeward/testworker/schema_digest.py
COPY --from=build /out/ /usr/local/bin/
COPY --from=build /seed/ /
COPY packaging/versions.json /opt/homeward/versions.json
WORKDIR /opt/homeward
USER 10001:10001
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/homeward-package", "serve"]
