# GitLab → GitHub 저장소 마이그레이션 도구

GitLab 프로젝트를 GitHub 저장소로 완전히 마이그레이션하는 엔터프라이즈급 명령줄 도구입니다.

## 주요 기능

* **완전한 저장소 마이그레이션**: 전체 커밋 히스토리, 브랜치, 태그 포함
* **풀 리퀘스트 마이그레이션**: GitLab 머지 리퀘스트를 GitHub 풀 리퀘스트로 변환
  - 열림/닫힘/병합된 요청 모두 포함
  - 댓글 및 토론 내용 보존
  - 원작성자 정보 유지
  - 승인 추적 (👍 반응으로 표시)
* **브랜치 관리**: `master` → `main` 브랜치 자동 이름 변경
* **보안 인증**: 토큰 기반 인증으로 자격 증명 노출 방지
* **동시 처리**: 설정 가능한 동시성으로 여러 프로젝트 병렬 마이그레이션
* **멱등성 연산**: 여러 번 실행해도 안전 - 기존 저장소 및 풀 리퀘스트 업데이트
* **엔터프라이즈 지원**: GitLab 자체 호스팅 및 GitHub Enterprise 인스턴스 지원

**현재 미지원 기능**: 이슈, 위키, 프로젝트 설정 등 GitLab 전용 기능 (기여 환영!)

## 시스템 요구사항

- Go 1.23 이상
- 유효한 GitLab 및 GitHub API 토큰
- GitLab 및 GitHub 인스턴스에 대한 네트워크 접근

## 설치

```bash
go install github.com/manicminer/gitlab-migrator
```

또는 소스에서 빌드:

```bash
git clone https://github.com/manicminer/gitlab-migrator.git
cd gitlab-migrator
go build -o gitlab-migrator
```

## 인증 설정

도구 사용 전 인증 토큰 설정:

```bash
export GITHUB_TOKEN="your_github_personal_access_token"
export GITLAB_TOKEN="your_gitlab_personal_access_token"
export GITHUB_USER="your_github_username"  # 선택사항, -github-user 플래그로도 설정 가능
```

**필수 토큰 권한:**
- **GitHub**: `repo`, `delete_repo` (-delete-existing-repos 사용 시)
- **GitLab**: `api`, `read_repository`

## 사용법

### 기본 마이그레이션

```bash
# 단일 프로젝트 마이그레이션
gitlab-migrator \
  -github-user=mytokenuser \
  -gitlab-project=mygitlabuser/myproject \
  -github-repo=mygithubuser/myrepo \
  -migrate-pull-requests

# master→main 브랜치 이름 변경과 함께 마이그레이션
gitlab-migrator \
  -github-user=mytokenuser \
  -gitlab-project=mygitlabuser/myproject \
  -github-repo=mygithubuser/myrepo \
  -migrate-pull-requests \
  -rename-master-to-main
```

### 대량 마이그레이션

```bash
# 프로젝트 매핑이 포함된 CSV 파일 생성
echo "gitlab-group/project1,github-org/repo1" > projects.csv
echo "gitlab-group/project2,github-org/repo2" >> projects.csv

# 여러 프로젝트 마이그레이션
gitlab-migrator \
  -github-user=mytokenuser \
  -projects-csv=projects.csv \
  -migrate-pull-requests \
  -max-concurrency=2
```

## 명령줄 옵션

```
  -delete-existing-repos
        마이그레이션 전 기존 저장소 삭제 여부
  -github-domain string
        사용할 GitHub 도메인 지정 (기본값: "github.com")
  -github-repo string
        마이그레이션할 GitHub 저장소
  -github-user string
        사용할 GitHub 사용자명 (마이그레이션된 PR의 작성자가 됨). GITHUB_USER 환경변수로도 설정 가능 (필수)
  -gitlab-domain string
        사용할 GitLab 도메인 지정 (기본값: "gitlab.com")
  -gitlab-project string
        마이그레이션할 GitLab 프로젝트
  -loop
        취소될 때까지 계속 마이그레이션
  -max-concurrency int
        병렬 마이그레이션할 프로젝트 수 (기본값: 4)
  -migrate-pull-requests
        풀 리퀘스트 마이그레이션 여부
  -projects-csv string
        마이그레이션할 프로젝트를 기술한 CSV 파일 경로 (-gitlab-project, -github-repo와 호환 불가)
  -rename-master-to-main
        master 브랜치를 main으로 이름 변경하고 풀 리퀘스트 업데이트
```

### 사용 방법

`-github-user` 인수로 인증 토큰이 발급된 GitHub 사용자명을 지정해야 합니다(필수). `GITHUB_USER` 환경변수로도 설정 가능합니다.

개별 GitLab 프로젝트는 `-gitlab-project` 인수로, 대상 GitHub 저장소는 `-github-repo` 인수로 지정합니다.

또는 `-projects-csv` 인수로 CSV 파일 경로를 제공할 수 있습니다. CSV 파일은 다음 형식이어야 합니다:

```csv
gitlab-group/gitlab-project-name,github-org-or-user/github-repo-name
```

인증을 위해 `GITLAB_TOKEN`과 `GITHUB_TOKEN` 환경변수를 설정해야 합니다. 토큰을 명령줄 인수로 지정할 수는 없습니다.

GitLab 머지 리퀘스트를 GitHub 풀 리퀘스트로 마이그레이션하려면(닫힌/병합된 것 포함!) `-migrate-pull-requests`를 지정합니다.

마이그레이션 전 기존 GitHub 저장소를 삭제하려면 `-delete-existing-repos` 인수를 전달합니다. _이는 위험할 수 있으며, 확인 메시지가 표시되지 않습니다._

**참고**: 대상 저장소가 존재하지 않으면 이 도구는 비공개 저장소를 생성하려고 시도합니다. 대상 저장소가 이미 존재하면 `-delete-existing-repos`를 지정하지 않는 한 기존 저장소가 사용됩니다.

자체 호스팅 GitLab 인스턴스는 `-gitlab-domain` 인수로, GitHub Enterprise 인스턴스는 `-github-domain` 인수로 위치를 지정합니다.

보너스 기능으로, 이 도구는 GitLab 저장소의 `master` 브랜치를 마이그레이션된 GitHub 저장소에서 `main`으로 투명하게 이름을 변경할 수 있습니다. `-rename-master-to-main` 인수로 활성화합니다.

기본적으로 최대 4개 프로젝트를 병렬로 마이그레이션하기 위해 4개의 워커가 생성됩니다. `-max-concurrency` 인수로 이를 증가시키거나 감소시킬 수 있습니다. GitHub API 속도 제한으로 인해 상당한 속도 향상을 경험하지 못할 수 있습니다. 자세한 내용은 [GitHub API 문서](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)를 참조하세요.

취소될 때까지 프로젝트 마이그레이션을 계속하려면 `-loop`를 지정합니다. 이는 마이그레이션 도구를 데몬화하거나 대량의 프로젝트(또는 소량의 매우 큰 프로젝트)를 마이그레이션할 때 자동으로 재시작하는 데 유용합니다.

## 고급 사용법

### 엔터프라이즈 인스턴스

```bash
# 자체 호스팅 GitLab 및 GitHub Enterprise
gitlab-migrator \
  -github-user=mytokenuser \
  -gitlab-domain=gitlab.mycompany.com \
  -github-domain=github.mycompany.com \
  -gitlab-project=mygroup/myproject \
  -github-repo=myorg/myrepo \
  -migrate-pull-requests
```

### 지속적 마이그레이션

```bash
# 지속적 동기화를 위한 연속 실행
gitlab-migrator \
  -github-user=mytokenuser \
  -projects-csv=projects.csv \
  -migrate-pull-requests \
  -loop
```

## 로깅

이 도구는 완전히 비대화형이며 구조화된 로깅 출력을 제공합니다. `LOG_LEVEL` 환경변수로 로깅 상세도를 설정합니다:

```bash
export LOG_LEVEL=DEBUG  # 옵션: ERROR, WARN, INFO, DEBUG, TRACE (기본값: INFO)
```

**로그 레벨:**
- `ERROR`: 치명적 오류만
- `WARN`: 경고 및 오류
- `INFO`: 일반 진행 정보 (기본값)
- `DEBUG`: 상세한 작업 정보
- `TRACE`: 매우 상세한 디버깅 출력

## 캐싱

이 도구는 API 요청 수를 줄이기 위해 특정 기본 요소에 대해 스레드 안전한 메모리 내 캐시를 유지합니다. 현재 다음 항목들이 처음 발견될 때 캐시되고, 이후 도구가 재시작될 때까지 캐시에서 검색됩니다:

- GitHub 풀 리퀘스트
- GitHub 이슈 검색 결과  
- GitHub 사용자 프로필
- GitLab 사용자 프로필

## 멱등성

이 도구는 멱등성을 가지려고 노력합니다. 반복해서 실행할 수 있으며 GitHub 저장소와 풀 리퀘스트를 GitLab에 있는 내용과 일치하도록 패치합니다. 이를 통해 큰 유지보수 윈도우 없이 여러 프로젝트를 마이그레이션할 수 있습니다.

_이 도구는 강제 미러 푸시를 수행하므로 대상 저장소에서 작업을 시작한 후에는 이 도구를 실행하는 것을 권장하지 않습니다._

풀 리퀘스트와 댓글의 경우, GitLab의 해당 ID가 마크다운 헤더에 추가되며, 이는 멱등성을 가능하게 하기 위해 파싱됩니다.

## 풀 리퀘스트

git 저장소는 그대로 마이그레이션되지만, 풀 리퀘스트는 GitHub API를 사용하여 관리되며 일반적으로 인증 토큰을 제공한 사람이 작성자가 됩니다.

각 풀 리퀘스트와 모든 댓글은 원래 작성자와 알아두면 유용한 기타 메타데이터를 보여주는 마크다운 테이블로 시작됩니다. 이는 또한 풀 리퀘스트와 댓글을 GitLab의 대응 항목에 매핑하는 데 사용되며 도구의 멱등성을 가능하게 합니다.

보너스로, GitLab 사용자가 GitLab 프로필의 `웹사이트` 필드에 GitHub 프로필 URL을 추가하면, 이 도구는 그들이 원래 작성한 PR이나 댓글의 마크다운 헤더에 GitHub 프로필 링크를 추가합니다.

이 도구는 또한 GitLab 프로젝트에서 병합/닫힌 머지 리퀘스트를 마이그레이션합니다. 각 저장소에서 임시 브랜치를 재구성하고, GitHub에 푸시하며, 풀 리퀘스트를 생성한 다음 닫고, 마지막으로 임시 브랜치를 삭제하는 방식으로 수행됩니다. 도구가 완료되면 저장소에 이러한 임시 브랜치가 없어야 합니다. 다만 GitHub는 즉시 가비지 컬렉션하지 않으므로 이러한 PR에서 `브랜치 복원` 버튼을 클릭할 수 있습니다.

## 아키텍처

이 도구는 현대적인 서비스 지향 아키텍처로 구축되었습니다:

- **서비스 계층**: 의존성 주입을 통한 중앙집중식 마이그레이션 서비스
- **모듈형 설계**: 프로젝트 마이그레이션, 풀 리퀘스트 처리, 인증을 위한 별도 모듈
- **보안 인증**: URL이나 로그에 자격 증명 노출 없음
- **스레드 안전 캐싱**: 동시 접근 보호가 있는 메모리 내 캐싱
- **오류 처리**: 포괄적인 오류 추적 및 보고
- **속도 제한**: 지수 백오프를 통한 내장 GitHub API 속도 제한 처리

## 문제 해결

### 일반적인 문제

**인증 오류**
```bash
# 토큰이 올바르게 설정되었는지 확인
echo $GITHUB_TOKEN | wc -c  # 40자 이상이어야 함
echo $GITLAB_TOKEN | wc -c  # 40자 이상이어야 함

# 토큰 접근 테스트
curl -H "Authorization: token $GITHUB_TOKEN" https://api.github.com/user
curl -H "Authorization: Bearer $GITLAB_TOKEN" https://gitlab.com/api/v4/user
```

**속도 제한**
- GitHub는 API 속도 제한이 있습니다 (인증된 사용자의 경우 시간당 5,000 요청)
- 도구에는 지수 백오프를 통한 자동 재시도가 포함되어 있습니다
- 대용량 마이그레이션의 경우 속도 제한 문제를 줄이기 위해 `-max-concurrency=1`을 사용하세요

**메모리 사용량**
- 대용량 저장소는 상당한 메모리를 소비할 수 있습니다
- 도구는 안전을 위해 메모리 내 Git 작업을 사용합니다
- 대형 프로젝트는 개별적으로 마이그레이션하는 것을 고려하세요

**네트워크 문제**
- GitLab 및 GitHub 인스턴스 모두에 대한 네트워크 접근을 확인하세요
- 엔터프라이즈 인스턴스의 경우 SSL 인증서 및 프록시 설정을 확인하세요

### 도움 받기

1. 디버그 로깅 활성화: `export LOG_LEVEL=DEBUG`
2. 특정 오류 메시지에 대한 로그 확인
3. 토큰 권한 및 만료 날짜 확인
4. 엔터프라이즈 인스턴스의 경우 도메인 접근성 확인

## 기여, 버그 신고 등...

GitHub 이슈 및 풀 리퀘스트를 사용해주세요. 이 프로젝트는 MIT 라이선스 하에 제공됩니다.

### 개발

```bash
# 클론 및 빌드
git clone https://github.com/manicminer/gitlab-migrator.git
cd gitlab-migrator
go mod download
go build -o gitlab-migrator

# 테스트 실행
go test ./...

# 코드 포맷팅
go fmt ./...
```