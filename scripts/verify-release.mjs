import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const read = path => readFileSync(new URL(`../${path}`, import.meta.url), 'utf8')
// 后端内置版本是运行时真源，其它发布消费者必须与它一致。
const version = read('backend/internal/config/config.go').match(/defaultAppVersion\s*=\s*"(v[^"]+)"/)?.[1]
assert.match(version ?? '', /^v\d+\.\d+\.\d+$/)
const packageJson = JSON.parse(read('frontend/package.json'))
const lock = JSON.parse(read('frontend/package-lock.json'))
assert.equal(`v${packageJson.version}`, version)
assert.equal(lock.version, packageJson.version)
assert.equal(lock.packages[''].version, packageJson.version)
const compose = read('deploy/docker-compose.prod.yml')
assert.ok(compose.includes(`image: transit-hub:${version}`))
assert.ok(compose.includes('pull_policy: build'))
assert.ok(compose.includes('dockerfile: deploy/Dockerfile'))
assert.ok(read('frontend/src/modules/admin/layout/AdminLayout.vue').includes("const githubRepoUrl = 'https://github.com/gwenliu1025/transit-hub'"))
for (const file of ['README.md', 'README_CN.md']) {
  const text = read(file)
  assert.ok(text.includes('git clone https://github.com/gwenliu1025/transit-hub.git'))
  assert.ok(!text.includes('deviseo/transithub:'))
  assert.ok(text.includes(`transit-hub:${version}`))
}
assert.ok(read(`docs/releases/${version}.md`).includes(version))
if (process.env.GITHUB_REF_TYPE === 'tag') assert.equal(process.env.GITHUB_REF_NAME, version)
console.log(`发布入口一致：${version}，默认从本仓库源码构建`)
