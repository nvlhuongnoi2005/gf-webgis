# Triển khai WebGIS trên Kubernetes

Thư mục này triển khai phần lõi ứng dụng vào namespace `webgis`:

- `frontend`: ứng dụng Vite tĩnh được Nginx phục vụ.
- `controller`: API xác thực Go và API gateway được bảo vệ.
- `auth-migrate`: Job migration cơ sở dữ liệu.

Database xác thực riêng được render độc lập từ `databases/auth/kustomization.yaml`.
`argocd/webgis-auth-db.yaml` khai báo Application Argo CD riêng, trong khi
Application WebGIS hiện có vẫn đồng bộ phần lõi `deployment/`.

Các worker bản đồ nằm trong `workers/workers.yaml` nhưng được chủ động tách khỏi
`kustomization.yaml`. Tile Server do Application Argo CD `webgis-tile-server`
quản lý độc lập, với source tại `workers/tile-server/`. Dữ liệu tile runtime nằm
ngoài đồng bộ Argo CD thông thường; phải import hoặc chuyển đổi vào PVC trước khi phục vụ.

## Bố cục

- `core/`: frontend, Controller, Job migration, ingress và NetworkPolicy lõi;
  được render bởi `kustomization.yaml` gốc cùng `namespace.yaml`.
- `databases/auth/`: Kustomization PostgreSQL xác thực tách biệt.
- `workers/`: worker bản đồ, storage worker và NetworkPolicy. Tile Server có
  Kustomization riêng tại `workers/tile-server/`.
- `operations/`: manifest import dữ liệu chạy một lần; không đưa vào Kustomization thông thường.
- `secrets/`: chỉ template được commit. Khóa tạo ra và Secret có giá trị bị Git bỏ qua.
- `overlays/` và `argocd/`: override cụm local và Application Argo CD.

## 1. Xây dựng và đẩy image

Thay `REGISTRY`, `TAG`, sau đó đặt chính xác cùng tên image trong
`controller.yaml` và `frontend.yaml`.

```powershell
docker build -f be/Dockerfile -t REGISTRY/webgis/controller:TAG .
docker build -f fe/Dockerfile -t REGISTRY/webgis/frontend:TAG .
docker push REGISTRY/webgis/controller:TAG
docker push REGISTRY/webgis/frontend:TAG
```

Controller được viết bằng Go. Khi phát triển local, cài Go 1.25 trở lên, mở lại
terminal để `go` có trong `PATH`, rồi chạy `npm start`.

Với Kubernetes local của Docker Desktop, có thể dùng image đã build tại máy mà
không cần `docker push` khi manifest dùng đúng tag và `imagePullPolicy: IfNotPresent`.

## 2. Tạo Secret ngoài Git

Tạo cặp khóa Ed25519 và các giá trị ngẫu nhiên, sau đó sao chép
`secret.example.yaml` thành `secret.yaml`, thay mọi placeholder và không commit
tệp này. `DATABASE_URL` phải dùng đúng mật khẩu của `POSTGRES_PASSWORD`.
Dùng mật khẩu an toàn với URL để không cần escape trong `DATABASE_URL`.

```powershell
go run ./be/cmd/keygen -out-dir deployment/secrets/.generated
Copy-Item deployment/secrets/secret.example.yaml deployment/secrets/secret.yaml
kubectl apply -f deployment/namespace.yaml
kubectl apply -f deployment/secrets/secret.yaml
```

Controller cần `AUTH_JWT_PRIVATE_KEY`, `AUTH_JWT_PUBLIC_KEY` và
`AUTH_REFRESH_TOKEN_PEPPER`; ở production, service chủ động không khởi động với
JWT key tạm thời.

## 3. Triển khai phần lõi

Với GitOps, giữ Application WebGIS hiện có trỏ tới thư mục này và chạy một lần
`kubectl apply -k deployment/argocd` để tạo Application database và Tile Server
độc lập. Job migration chờ `auth-postgres`, chạy SQL migration, rồi sync wave
cập nhật Controller. CI publish Controller image (có SQL migration) và cập nhật
tag image của Controller lẫn Job migration khi backend thay đổi.

Với lần triển khai thủ công đầu tiên, apply tài nguyên và chờ Postgres trước khi
chạy lại Job migration:

```powershell
kubectl apply -f deployment/namespace.yaml
kubectl apply -k deployment/databases/auth
kubectl -n webgis rollout status statefulset/auth-postgres
kubectl apply -k deployment
kubectl -n webgis delete job auth-migrate --ignore-not-found
kubectl -n webgis apply -f deployment/core/migration-job.yaml
kubectl -n webgis wait --for=condition=complete job/auth-migrate --timeout=180s
kubectl -n webgis rollout status deployment/controller
kubectl -n webgis rollout status deployment/frontend
```

Job migration nằm trong `kustomization.yaml` lõi và có init container chờ tối đa
mười phút cho database được quản lý độc lập.

## 4. Ingress và truy cập local

Cài ingress-nginx controller đã ghim phiên bản một lần cho mỗi Kubernetes cluster:

```powershell
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.15.1/deploy/static/provider/cloud/deploy.yaml
kubectl -n ingress-nginx rollout status deployment/ingress-nginx-controller --timeout=300s
```

Khi kiểm thử local bằng Docker Desktop, apply manifest thông thường trước, rồi
apply local overlay. Overlay định tuyến `http://webgis.localhost` qua ingress-nginx
và chỉ tắt secure cookie cho môi trường HTTP local này.

```powershell
kubectl apply -k deployment
kubectl apply -k deployment/overlays/local
kubectl -n webgis rollout restart deployment/controller
kubectl -n webgis rollout status deployment/controller
```

Mở `http://webgis.localhost`. Trình duyệt hiện đại phân giải `*.localhost` về
máy local. Nếu trình duyệt không làm vậy, thêm `127.0.0.1 webgis.localhost` vào
hosts file Windows. Không cần port-forward khi ingress-nginx đang chạy.

Ở production, giữ `ingress.yaml` base, thay `webgis.example.com` bằng domain thật,
cấu hình TLS/cert-manager, đặt `APP_ORIGINS` thành HTTPS origin và giữ
`AUTH_SECURE_COOKIES=true`.

Để smoke test local không dùng Ingress:

```powershell
kubectl -n webgis port-forward service/frontend 8088:80
kubectl -n webgis port-forward service/controller 3001:3001
```

Mở `http://localhost:8088`. Frontend Nginx proxy `/auth` và `/api` tới Controller
trong cluster, do đó catalog bản đồ và tile giữ cùng đường dẫn như production.
Không nên public trực tiếp Controller hoặc worker qua `LoadBalancer`/`NodePort` ở production.

## 5. Chuyển worker bản đồ từ Docker

Dữ liệu Docker hiện tại đo được khoảng 393 MB cho Tile Server, 2.9 GB cho thư
mục bản đồ dùng chung của Valhalla/Nominatim và 10 GB cho Nominatim PostGIS.
Manifest lần lượt dùng PVC 5 Gi, 10 Gi và 30 Gi; chúng được chủ động tách khỏi
`kustomization.yaml`.

Xây dựng và đẩy image Nominatim local với cùng release tag của Controller và frontend:

```powershell
docker build -f ../Nominatim/Dockerfile -t ghcr.io/nvlhuongnoi2005/webgis/nominatim:26.9 ../Nominatim
docker push ghcr.io/nvlhuongnoi2005/webgis/nominatim:26.9
```

Tạo Secret database không commit. `POSTGRES_PASSWORD` và mật khẩu trong
`NOMINATIM_DATABASE_DSN` phải hoàn toàn giống nhau.

```powershell
$bytes = New-Object byte[] 32
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
$rng.Dispose()
$nominatimDbPassword = [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')

kubectl -n webgis create secret generic nominatim-db-credentials `
  --from-literal=POSTGRES_USER=nominatim `
  --from-literal=POSTGRES_PASSWORD=$nominatimDbPassword `
  --from-literal=NOMINATIM_DATABASE_DSN="pgsql:dbname=nominatim;host=nominatim-db;port=5432;user=nominatim;password=$nominatimDbPassword" `
  --dry-run=client -o yaml | kubectl apply -f -
```

Tạo PVC và database Nominatim đích, sau đó chờ database sẵn sàng:

```powershell
kubectl -n webgis apply -f deployment/workers/worker-storage.yaml
kubectl -n webgis apply -f deployment/workers/tile-server/storage.yaml
kubectl -n webgis rollout status statefulset/nominatim-db --timeout=300s
kubectl -n webgis exec nominatim-db-0 -- createdb -U nominatim nominatim
kubectl -n webgis apply -f deployment/operations/worker-importer.yaml
kubectl -n webgis apply -f deployment/workers/tile-server/operations/importer.yaml
kubectl -n webgis wait --for=condition=Ready pod/import-tile-data pod/import-map-data pod/import-nominatim-project --timeout=180s
```

Các lệnh sau stream dữ liệu từ Docker container đang chạy vào PVC; chúng không
tạo thêm một bản sao local. Giữ container nguồn chạy cho đến khi từng lệnh thành công.

```powershell
cmd.exe /d /s /c "docker cp tile-server:/tile-server/data - | kubectl -n webgis exec -i import-tile-data -- tar -C /target --strip-components=1 -xf -"
cmd.exe /d /s /c "docker cp valhalla:/custom_files - | kubectl -n webgis exec -i import-map-data -- tar -C /target --strip-components=1 -xf -"
cmd.exe /d /s /c "docker cp nominatim-nominatim-1:/nominatim-project - | kubectl -n webgis exec -i import-nominatim-project -- tar -C /target --strip-components=1 -xf -"
cmd.exe /d /s /c "docker exec nominatim-db-1 pg_dump -U nominatim -Fc nominatim | kubectl -n webgis exec -i nominatim-db-0 -- pg_restore -U nominatim -d nominatim --no-owner"
```

Xác minh database đã import trước khi chạy API. Sau đó xóa pod importer tạm thời
và khởi động ba worker cùng NetworkPolicy private:

```powershell
kubectl -n webgis exec nominatim-db-0 -- psql -U nominatim -d nominatim -c "SELECT count(*) FROM placex;"
# Chỉ cần khi database nguồn cũ cũng từng được dùng cho xác thực WebGIS.
kubectl -n webgis exec nominatim-db-0 -- psql -U nominatim -d nominatim -c "DROP TABLE IF EXISTS auth_event_checkpoints, auth_events, refresh_token_history, user_quotas, sessions, users; DROP FUNCTION IF EXISTS set_auth_updated_at();"
kubectl -n webgis delete -f deployment/operations/worker-importer.yaml
kubectl -n webgis delete -f deployment/workers/tile-server/operations/importer.yaml
kubectl -n webgis apply -f deployment/workers/tile-server/operations/assign-dataset-roles.yaml
kubectl -n webgis apply -k deployment/workers/tile-server
kubectl -n webgis apply -f deployment/workers/worker-networkpolicy.yaml
kubectl -n webgis apply -f deployment/workers/workers.yaml
kubectl -n webgis rollout status deployment/tile-server --timeout=300s
kubectl -n webgis rollout status deployment/valhalla --timeout=600s
kubectl -n webgis rollout status deployment/nominatim --timeout=300s
```

Mọi Service worker đều là `ClusterIP`; client không thể gọi trực tiếp. Controller
proxy tile bản đồ, request tìm kiếm Nominatim công khai và request định tuyến đã
xác thực tới Valhalla tại `valhalla:8002`. Tile và Nominatim cần route Controller
có xác thực riêng trước khi được frontend public ở production.

## Vận hành

```powershell
kubectl -n webgis get pods,svc,pvc,ingress
kubectl -n webgis logs deployment/controller --tail=100
kubectl -n webgis logs job/auth-migrate
kubectl -n webgis describe pod -l app.kubernetes.io/name=controller
```

Database auth được chủ động tách khỏi database Nominatim. Điều này tránh gắn dữ
liệu tài khoản với dataset geocoding import và cho Controller truy cập qua
Service `auth-postgres` ClusterIP riêng tư.

## Đồng bộ POI cho tìm kiếm không gian

Sau khi Job `auth-migrate` hoàn tất và database Nominatim sẵn sàng, sao chép các
nhóm POI được hỗ trợ vào database ứng dụng:

```powershell
kubectl -n webgis delete job poi-sync --ignore-not-found
kubectl -n webgis apply -f deployment/workers/poi-sync.yaml
kubectl -n webgis wait --for=condition=complete job/poi-sync --timeout=600s
```

Job đọc Nominatim qua Service riêng tư `nominatim-db` và ghi vào bảng
`search_pois` do sản phẩm sở hữu trong `auth-postgres`. Chạy lại sau mỗi lần
import dữ liệu Nominatim/OSM.
