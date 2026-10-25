# PV 관리 라벨 패치 및 재검증 전 점검

## 적용 범위
새 PV Work의 embedded PersistentVolume에 karmada.io/managed=true를 넣는다.
Work 이름/namespace routing annotation과 기존 migration UID 라벨은 유지한다.
스토리지 spec, PVC, NFS 데이터는 수정하지 않는다.

이 변경은 planHash에 포함된다. 이전 버전으로 시작한 진행 중 PVMigration을
새 이미지로 자동 채택하지 않는다. 이번 실행처럼 Verified 증거를 보관하고
이전 PVMigration을 정상 종료한 뒤 새 실행에 적용한다.
진행 중 작업이 있으면 배포를 중단한다. 기존 Dockerfile 수정은 보존한다.

## MGMT에서 소스 갱신과 Buildah 배포
다른 namespace의 진행 중 작업도 확인한다. 단순히 현재 namespace가 비었다고
전체 배포가 안전하다고 가정하지 않는다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" get pvmigrations -A
cd /root/hybridspot-validation/pv
git status --short
git diff -- Dockerfile
git pull --ff-only
git log -1 --oneline
grep -n 'karmada.io/managed' internal/management/migration.go
```

pull이 로컬 변경 충돌로 실패하면 중단한다. reset/checkout으로 Dockerfile을 버리지 않는다.
아래 빌드는 새로운 태그를 사용한다. 빌드/푸시 실패 시 배포하지 않는다.

```bash
(
set -euo pipefail
cd /root/hybridspot-validation/pv
TAG="managed-pv-$(git rev-parse --short HEAD)"
IMAGE="docker.io/jeongseungjun/multicluster-pvmigration-system:$TAG"
buildah bud --layers -f Dockerfile -t "$IMAGE" .
buildah push "$IMAGE" "docker://$IMAGE"
kubectl --context=kubernetes -n pv-migration-system \
  set image deployment/pv-migration-management manager="$IMAGE"
kubectl --context=kubernetes -n pv-migration-system \
  rollout status deployment/pv-migration-management --timeout=180s
)
```

## 별도 pv-work 생성 경로: 로컬 소스에서 확인한 사실
보관된 Karmada_with_PVMigration 소스의
pkg/controllers/pvsync/pvmigration_controller.go에서 pv-work 이름과
pvsync.karmada.io 라벨을 생성한다.
prefixes 배열과 templates 배열을 별도로 만든 뒤 인덱스로 결합하는 코드가 있어,
claim template 이름을 키로 일대일 대응시키는 현재 standalone PV 시스템과 다르다.
실제 오류는 dataset PV에 checkpoint NFS 경로가 들어간 것이다.
로컬 소스와 배포 바이너리의 일치 여부는 아직 확인되지 않았다.

해당 로컬 소스의 cmd/controller-manager/app/controllermanager.go 등록 키:
pvSync, pvMigration, pvCleanup.
로그 controller 이름과 --controllers 등록 키를 혼동하지 않는다.

먼저 MGMT에서 실제 배포 command/args 및 바이너리 도움말을 조회한다.
아래 exec는 배포 command가 기본 karmada-controller-manager 바이너리일 때 사용한다.

```bash
kubectl --context=kubernetes -n karmada-system \
  get deployment karmada-controller-manager -o json |
jq '.spec.template.spec.containers[] | {name,image,command,args}'
kubectl --context=kubernetes -n karmada-system \
  exec deployment/karmada-controller-manager -- karmada-controller-manager --help
```

출력에서 pvSync/pvMigration/pvCleanup이 지원되는지 확인한다.
기존 --controllers 설정을 보존하면서 세 구형 컨트롤러만 제외해야 한다.
기존 설정이 기본 *이고 세 이름이 실제 지원될 때의 후보는
--controllers=*,-pvSync,-pvMigration,-pvCleanup 이다.
도움말 확인 없이 이 값을 배포하지 않는다. 공용 Karmada 컨트롤러 변경은
다른 legacy workload에 영향을 주므로 활성 legacy 작업이 없는지도 확인한다.
standalone pv-migration-management는 중지하지 않는다.
전체 Karmada Deployment를 scale 0 하거나 정체가 불분명한 이미지를 교체하지 않는다.

## 재실행 통과 조건
- 기존 demo Pod/복원 작업은 종료, source GPU uncordon.
- 구형 PV 생성 경로와 standalone 경로가 동일 workload를 동시에 관리하지 않음.
- 새 workload 이름과 checkpoint 경로 사용. 기존 PVC/PV/NFS는 유지.
- 새 Work PV manifest와 실제 AWS PV에 관리 라벨과 routing annotation 존재.
- 수동 라벨 추가 없이 Applied=True 및 PV 상태 반영.
- PVMigration Completed, 대상 PVC Bound, RestoreRequest Verified.
- 원본 checkpoint step과 대상 restore 이벤트 대조, 이후 step 증가.
- 중간 수동 보정이 발생하면 무개입 재검증으로 기록하지 않음.

클러스터 배포, controller 비활성화 및 실제 GPU 재실행은 이 문서 작성 시 실행하지 않았다.

