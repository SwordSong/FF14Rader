#!/usr/bin/env node
// HTTP analysis server for xivanalysis. Built-in http only; no external deps.
// POST /analyze with payload {code, report, fightId, events} or {requests:[...]}

const http = require('http')
const fs = require('fs')
const os = require('os')
const path = require('path')
const Module = require('module')
const {fork} = require('child_process')
const {Worker, isMainThread, parentPort} = require('worker_threads')

const repoRoot = path.resolve(__dirname, '..')
const xivaRoot = path.resolve(repoRoot, 'external', 'xivanalysis')
const xivaSrc = path.resolve(xivaRoot, 'src')
const DEFAULT_HOSTS_CONFIG_PATH = path.join(repoRoot, 'docs', 'xiva-hosts.json')
const HOSTS_CONFIG_PATH = resolveHostsConfigPath(process.env.XIVA_HOSTS_CONFIG)

loadEnvFiles([
  path.join(repoRoot, '.env'),
  path.join(repoRoot, 'external', '.env'),
])

const PORT = Number(process.env.PORT || process.env.port || 22026) || 3000
const HOST = process.env.HOST || '0.0.0.0'
const MAX_BODY_BYTES = Number(process.env.MAX_BODY_BYTES || 20 * 1024 * 1024) || 20 * 1024 * 1024
const EXECUTION_MODE = (process.env.XIVA_EXECUTION_MODE || '').toLowerCase()
const THREAD_WORKER_MODE = process.env.XIVA_THREAD_WORKER === '1'
const CALL_CONCURRENCY_PER_WORKER = parsePositiveInt(process.env.XIVA_CALL_CONCURRENCY, 1)
const ANALYZE_TIMEOUT_MS = Number(process.env.ANALYZE_TIMEOUT_MS || 90 * 1000) || 90 * 1000
const SHUTDOWN_TIMEOUT_MS = Number(process.env.SHUTDOWN_TIMEOUT_MS || 30 * 1000) || 30 * 1000
const API_KEY = process.env.XIVA_API_KEY || process.env.API_KEY || ''
const LOG_LEVEL = (process.env.LOG_LEVEL || 'info').toLowerCase()
const OUTPUT_MODE = (process.env.XIVA_OUTPUT_MODE || 'numeric').toLowerCase()
const OUTPUT_LOCALE = (process.env.XIVA_OUTPUT_LOCALE || process.env.XIVA_LOCALE || 'zh').toLowerCase()
const WORKER_MODE = process.env.XIVA_WORKER_MODE === '1'
const PORT_POOL_START = parsePositiveInt(process.env.XIVA_PORT_START, 22000)
const PORT_POOL_COUNT = parsePositiveInt(process.env.XIVA_PORT_COUNT, 1)
const THREAD_POOL_AUTOSCALE_INTERVAL_MS = Math.max(
  1000,
  Number(process.env.XIVA_THREAD_POOL_AUTOSCALE_INTERVAL_MS || 15 * 1000) || 15 * 1000,
)
const THREAD_POOL_RECOMMENDATION_INFO = computeRecommendedThreadPoolSize()
const THREAD_POOL_SIZE = THREAD_POOL_RECOMMENDATION_INFO.recommended
const THREAD_POOL_SIZE_SOURCE = `recommended:${THREAD_POOL_RECOMMENDATION_INFO.reason}`
const RECOMMENDED_CONCURRENCY_INFO = computeLocalRecommendedConcurrency(THREAD_POOL_SIZE)
const EXPLICIT_MAX_CONCURRENCY = parsePositiveInt(process.env.MAX_CONCURRENCY, 0)
const DEFAULT_MAX_CONCURRENCY = EXPLICIT_MAX_CONCURRENCY > 0 ? EXPLICIT_MAX_CONCURRENCY : RECOMMENDED_CONCURRENCY_INFO.recommended
const DEFAULT_MAX_CONCURRENCY_SOURCE = EXPLICIT_MAX_CONCURRENCY > 0
  ? 'env:MAX_CONCURRENCY'
  : `recommended:${RECOMMENDED_CONCURRENCY_INFO.reason}`

const RECOMMEND_REASON_ZH = {
  'single-default': '单机默认策略',
  'thread-pool-default': '线程池默认策略',
  'host-performance': '主机性能评估',
  'thread-pool-autoscale': '线程池自动伸缩联动',
  'cpu-parallelism': 'CPU 并行度',
  'hosts-config': '解析主机配置',
  'execution-mode': '执行模式配置',
  'hosts-config-local': '本地解析主机配置',
  'port-pool': '端口池配置',
}

const MODE_ZH = {
  threads: '线程池模式',
  'process-worker': '进程工作节点',
  single: '单进程模式',
}

function loadEnvFiles(files) {
  for (const filePath of files) {
    if (!fs.existsSync(filePath)) {
      continue
    }
    loadEnvFile(filePath)
  }
}

function loadEnvFile(filePath) {
  const raw = fs.readFileSync(filePath, 'utf8')
  const lines = raw.split(/\r?\n/)
  for (const line of lines) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) {
      continue
    }

    const idx = trimmed.indexOf('=')
    if (idx <= 0) {
      continue
    }

    const key = trimmed.slice(0, idx).trim()
    if (!key || process.env[key] != null) {
      continue
    }

    let value = trimmed.slice(idx + 1).trim()
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1)
    }

    process.env[key] = value
  }
}

function parsePositiveInt(raw, fallback) {
  const parsed = Number(raw)
  if (!Number.isFinite(parsed) || parsed <= 0) {
    return fallback
  }
  return Math.floor(parsed)
}

function clampInt(value, min, max) {
  if (value < min) {
    return min
  }
  if (value > max) {
    return max
  }
  return value
}

function clampFloat(value, min, max) {
  const n = Number(value)
  if (!Number.isFinite(n)) {
    return min
  }
  if (n < min) {
    return min
  }
  if (n > max) {
    return max
  }
  return n
}

function resolveHostsConfigPath(rawPath) {
  const trimmed = typeof rawPath === 'string' ? rawPath.trim() : ''
  if (trimmed === '') {
    return DEFAULT_HOSTS_CONFIG_PATH
  }
  if (path.isAbsolute(trimmed)) {
    return trimmed
  }
  return path.resolve(repoRoot, trimmed)
}

function getCpuParallelism() {
  if (typeof os.availableParallelism === 'function') {
    return parsePositiveInt(os.availableParallelism(), 1)
  }
  const cpus = os.cpus()
  if (Array.isArray(cpus) && cpus.length > 0) {
    return cpus.length
  }
  return 1
}

function loadAnalyzeHostEntriesFromConfig(configPath) {
  if (!configPath || !fs.existsSync(configPath)) {
    return []
  }

  try {
    const raw = fs.readFileSync(configPath, 'utf8')
    const parsed = JSON.parse(raw)

    if (Array.isArray(parsed)) {
      return parsed
    }
    if (parsed && Array.isArray(parsed.servers)) {
      return parsed.servers
    }
  } catch (_err) {
    return []
  }

  return []
}

function getLocalHostSet() {
  const set = new Set(['localhost', '127.0.0.1', '::1', '0:0:0:0:0:0:0:1'])
  const nets = os.networkInterfaces()
  for (const values of Object.values(nets || {})) {
    if (!Array.isArray(values)) {
      continue
    }
    for (const info of values) {
      if (!info || typeof info.address !== 'string' || info.address.trim() === '') {
        continue
      }
      set.add(info.address.trim().toLowerCase())
    }
  }
  return set
}

function extractHostFromAnalyzeEntry(entry) {
  if (typeof entry === 'string') {
    return extractHostFromRawEndpoint(entry)
  }
  if (!entry || typeof entry !== 'object') {
    return ''
  }

  if (typeof entry.url === 'string' && entry.url.trim() !== '') {
    return extractHostFromRawEndpoint(entry.url)
  }

  const host = typeof entry.host === 'string' ? entry.host.trim() : ''
  if (host === '') {
    return ''
  }
  return host.toLowerCase()
}

function extractHostFromRawEndpoint(raw) {
  const trimmed = typeof raw === 'string' ? raw.trim() : ''
  if (trimmed === '') {
    return ''
  }

  let normalized = trimmed
  if (!normalized.includes('://')) {
    normalized = `http://${normalized}`
  }

  try {
    const u = new URL(normalized)
    return (u.hostname || '').trim().toLowerCase()
  } catch (_err) {
    return ''
  }
}

function isLocalHost(host, localHostSet) {
  const h = typeof host === 'string' ? host.trim().toLowerCase() : ''
  if (h === '') {
    return false
  }
  if (localHostSet.has(h)) {
    return true
  }
  if (h.startsWith('127.')) {
    return true
  }
  return false
}

function summarizeHostSlots(hostEntries, localHostSet) {
  let totalHostSlots = 0
  let localHostSlots = 0

  for (const entry of hostEntries) {
    if (entry && typeof entry === 'object' && entry.enabled === false) {
      continue
    }

    const host = extractHostFromAnalyzeEntry(entry)
    if (!host) {
      continue
    }

    const weight = entry && typeof entry === 'object'
      ? parsePositiveInt(entry.weight, 1)
      : 1

    totalHostSlots += weight
    if (isLocalHost(host, localHostSet)) {
      localHostSlots += weight
    }
  }

  return {totalHostSlots, localHostSlots}
}

function computeRecommendedThreadPoolSize() {
  const localHostSet = getLocalHostSet()
  const hostEntries = loadAnalyzeHostEntriesFromConfig(HOSTS_CONFIG_PATH)
  const slots = summarizeHostSlots(hostEntries, localHostSet)
  const cpuParallelism = clampInt(getCpuParallelism(), 1, 64)
  const loadAvg1 = clampFloat(os.loadavg()[0], 0, 9999)
  const loadPerCore = cpuParallelism > 0 ? loadAvg1 / cpuParallelism : 0

  const memoryTotalBytes = Math.max(1, Number(os.totalmem()) || 1)
  const memoryFreeBytes = clampFloat(os.freemem(), 0, memoryTotalBytes)
  const memoryFreeRatio = clampFloat(memoryFreeBytes / memoryTotalBytes, 0, 1)

  // Conservative down-scaling when host load or memory pressure is high.
  let cpuFactor = 1
  if (loadPerCore >= 1.5) {
    cpuFactor = 0.45
  } else if (loadPerCore >= 1.2) {
    cpuFactor = 0.6
  } else if (loadPerCore >= 1.0) {
    cpuFactor = 0.75
  } else if (loadPerCore >= 0.75) {
    cpuFactor = 0.9
  }

  let memoryFactor = 1
  if (memoryFreeRatio <= 0.1) {
    memoryFactor = 0.45
  } else if (memoryFreeRatio <= 0.2) {
    memoryFactor = 0.65
  } else if (memoryFreeRatio <= 0.3) {
    memoryFactor = 0.8
  }

  const rawRecommended = Math.floor(cpuParallelism * cpuFactor * memoryFactor)
  const recommended = clampInt(rawRecommended, 1, cpuParallelism)
  const reason = 'host-performance'

  return {
    recommended,
    reason,
    cpuCap: cpuParallelism,
    cpuParallelism,
    loadAvg1,
    loadPerCore,
    cpuFactor,
    memoryTotalBytes,
    memoryFreeBytes,
    memoryFreeRatio,
    memoryFactor,
    portPoolCount: PORT_POOL_COUNT,
    hostsConfigPath: HOSTS_CONFIG_PATH,
    hostEntries: hostEntries.length,
    totalHostSlots: slots.totalHostSlots,
    localHostSlots: slots.localHostSlots,
  }
}

function computeLocalRecommendedConcurrency(threadPoolSize) {
  const perWorker = Math.max(1, CALL_CONCURRENCY_PER_WORKER)
  const localHostSet = getLocalHostSet()
  const hostEntries = loadAnalyzeHostEntriesFromConfig(HOSTS_CONFIG_PATH)
  const slots = summarizeHostSlots(hostEntries, localHostSet)

  let reason = 'single-default'
  let rawRecommended = 0
  const mode = EXECUTION_MODE
  const isThreadLikeMode = mode === 'thread' || mode === 'threads' || mode === 'process' || mode === 'processes' || mode === 'pool'

  if (isThreadLikeMode && threadPoolSize > 0) {
    rawRecommended = threadPoolSize * perWorker
    reason = 'execution-mode'
  } else if (slots.localHostSlots > 0) {
    rawRecommended = slots.localHostSlots * perWorker
    reason = 'hosts-config-local'
  } else if (PORT_POOL_COUNT > 1) {
    rawRecommended = PORT_POOL_COUNT * perWorker
    reason = 'port-pool'
  } else {
    rawRecommended = Math.max(1, threadPoolSize) * perWorker
  }

  const cpuCap = clampInt(getCpuParallelism() * 2, 1, 128)
  const recommended = clampInt(rawRecommended, 1, cpuCap)

  return {
    recommended,
    reason,
    cpuCap,
    perWorker,
    threadPoolSize,
    portPoolCount: PORT_POOL_COUNT,
    hostsConfigPath: HOSTS_CONFIG_PATH,
    hostEntries: hostEntries.length,
    totalHostSlots: slots.totalHostSlots,
    localHostSlots: slots.localHostSlots,
  }
}

function toReasonZh(reason) {
  return RECOMMEND_REASON_ZH[reason] || reason || '未知来源'
}

function toModeZh(mode) {
  return MODE_ZH[mode] || mode || '未知模式'
}

function formatBytesZh(bytes) {
  const n = Number(bytes)
  if (!Number.isFinite(n) || n < 0) {
    return '未知'
  }
  const kb = 1024
  const mb = kb * 1024
  if (n >= mb) {
    return `${(n / mb).toFixed(2)} MB`
  }
  if (n >= kb) {
    return `${(n / kb).toFixed(2)} KB`
  }
  return `${Math.floor(n)} B`
}

function toMaxConcurrencySourceZh(source) {
  return toConfigSourceZh(source)
}

function toThreadPoolSourceZh(source) {
  return toConfigSourceZh(source)
}

function toConfigSourceZh(source) {
  const text = typeof source === 'string' ? source : ''
  if (text.startsWith('env:')) {
    return `环境变量覆盖（${text.slice(4)}）`
  }
  if (text.startsWith('recommended:')) {
    return `推荐值（${toReasonZh(text.slice('recommended:'.length))}）`
  }
  if (text) {
    return text
  }
  return '未知来源'
}

function buildRecommendedConcurrencyZh(info) {
  if (!info || typeof info !== 'object') {
    return undefined
  }
  return {
    推荐并发: info.recommended,
    推荐原因: toReasonZh(info.reason),
    CPU上限: info.cpuCap,
    每工作单元并发: info.perWorker,
    线程池大小: info.threadPoolSize,
    端口池数量: info.portPoolCount,
    主机配置路径: info.hostsConfigPath,
    配置主机条目数: info.hostEntries,
    主机总槽位: info.totalHostSlots,
    本地主机槽位: info.localHostSlots,
  }
}

function buildRecommendedThreadPoolZh(info) {
  if (!info || typeof info !== 'object') {
    return undefined
  }
  return {
    推荐线程池大小: info.recommended,
    推荐原因: toReasonZh(info.reason),
    CPU并行度: info.cpuParallelism,
    当前1分钟负载: info.loadAvg1,
    每核负载: info.loadPerCore,
    CPU调整系数: info.cpuFactor,
    内存可用比例: info.memoryFreeRatio,
    内存调整系数: info.memoryFactor,
    CPU上限: info.cpuCap,
    端口池数量: info.portPoolCount,
    主机配置路径: info.hostsConfigPath,
    配置主机条目数: info.hostEntries,
    主机总槽位: info.totalHostSlots,
    本地主机槽位: info.localHostSlots,
  }
}

function getThreadPoolConfigInfo() {
  if (threadPoolManager && typeof threadPoolManager.getSizingInfo === 'function') {
    return threadPoolManager.getSizingInfo()
  }

  return {
    threadPoolSize: THREAD_POOL_SIZE,
    threadPoolSizeSource: THREAD_POOL_SIZE_SOURCE,
    recommendedThreadPool: THREAD_POOL_RECOMMENDATION_INFO,
    autoScaleIntervalMs: THREAD_POOL_AUTOSCALE_INTERVAL_MS,
  }
}

function buildDynamicRecommendedConcurrency(poolInfo) {
  const threadPoolSize = parsePositiveInt(poolInfo && poolInfo.threadPoolSize, 1)
  const perWorker = Math.max(1, CALL_CONCURRENCY_PER_WORKER)
  const dynamicRecommended = clampInt(threadPoolSize * perWorker, 1, 128)

  return {
    ...RECOMMENDED_CONCURRENCY_INFO,
    recommended: dynamicRecommended,
    reason: 'thread-pool-autoscale',
    perWorker,
    threadPoolSize,
  }
}

function getCurrentConcurrencyInfo(threadPoolConfigInfo) {
  if (EXPLICIT_MAX_CONCURRENCY > 0) {
    return {
      maxConcurrency: EXPLICIT_MAX_CONCURRENCY,
      maxConcurrencySource: 'env:MAX_CONCURRENCY',
      recommendedConcurrency: RECOMMENDED_CONCURRENCY_INFO,
    }
  }

  const poolInfo = threadPoolConfigInfo || getThreadPoolConfigInfo()
  if (threadPoolManager && poolInfo && parsePositiveInt(poolInfo.threadPoolSize, 0) > 0) {
    const recommendedConcurrency = buildDynamicRecommendedConcurrency(poolInfo)
    return {
      maxConcurrency: recommendedConcurrency.recommended,
      maxConcurrencySource: 'recommended:thread-pool-autoscale',
      recommendedConcurrency,
    }
  }

  return {
    maxConcurrency: DEFAULT_MAX_CONCURRENCY,
    maxConcurrencySource: DEFAULT_MAX_CONCURRENCY_SOURCE,
    recommendedConcurrency: RECOMMENDED_CONCURRENCY_INFO,
  }
}

function buildConcurrencyCompatFields(mode, threadPoolConfigInfo, concurrencyInfo) {
  const poolInfo = threadPoolConfigInfo || getThreadPoolConfigInfo()
  const runtimeConcurrency = concurrencyInfo || getCurrentConcurrencyInfo(poolInfo)
  return {
    modeZh: toModeZh(mode),
    maxBodyBytesZh: formatBytesZh(MAX_BODY_BYTES),
    threadPoolSizeZh: poolInfo.threadPoolSize,
    threadPoolSizeSourceZh: toThreadPoolSourceZh(poolInfo.threadPoolSizeSource),
    recommendedThreadPoolZh: buildRecommendedThreadPoolZh(poolInfo.recommendedThreadPool),
    threadPoolAutoScaleIntervalMsZh: poolInfo.autoScaleIntervalMs,
    maxConcurrencyZh: runtimeConcurrency.maxConcurrency,
    maxConcurrencySourceZh: toMaxConcurrencySourceZh(runtimeConcurrency.maxConcurrencySource),
    recommendedConcurrencyZh: buildRecommendedConcurrencyZh(runtimeConcurrency.recommendedConcurrency),
  }
}

const LEVEL_WEIGHT = {
  debug: 10,
  info: 20,
  warn: 30,
  error: 40,
}

const OPTIONAL_DEPENDENCY_HANDLES = new Set([
  'cooldownDowntime',
])

const JOB_KEY_MAP = {
  PALADIN: {reportJob: 'PALADIN', moduleJob: 'PLD'},
  PLD: {reportJob: 'PALADIN', moduleJob: 'PLD'},
  WARRIOR: {reportJob: 'WARRIOR', moduleJob: 'WAR'},
  WAR: {reportJob: 'WARRIOR', moduleJob: 'WAR'},
  DARKKNIGHT: {reportJob: 'DARK_KNIGHT', moduleJob: 'DRK'},
  DARK_KNIGHT: {reportJob: 'DARK_KNIGHT', moduleJob: 'DRK'},
  DRK: {reportJob: 'DARK_KNIGHT', moduleJob: 'DRK'},
  GUNBREAKER: {reportJob: 'GUNBREAKER', moduleJob: 'GNB'},
  GNB: {reportJob: 'GUNBREAKER', moduleJob: 'GNB'},

  WHITEMAGE: {reportJob: 'WHITE_MAGE', moduleJob: 'WHM'},
  WHITE_MAGE: {reportJob: 'WHITE_MAGE', moduleJob: 'WHM'},
  WHM: {reportJob: 'WHITE_MAGE', moduleJob: 'WHM'},
  SCHOLAR: {reportJob: 'SCHOLAR', moduleJob: 'SCH'},
  SCH: {reportJob: 'SCHOLAR', moduleJob: 'SCH'},
  ASTROLOGIAN: {reportJob: 'ASTROLOGIAN', moduleJob: 'AST'},
  AST: {reportJob: 'ASTROLOGIAN', moduleJob: 'AST'},
  SAGE: {reportJob: 'SAGE', moduleJob: 'SGE'},
  SGE: {reportJob: 'SAGE', moduleJob: 'SGE'},

  MONK: {reportJob: 'MONK', moduleJob: 'MNK'},
  MNK: {reportJob: 'MONK', moduleJob: 'MNK'},
  DRAGOON: {reportJob: 'DRAGOON', moduleJob: 'DRG'},
  DRG: {reportJob: 'DRAGOON', moduleJob: 'DRG'},
  NINJA: {reportJob: 'NINJA', moduleJob: 'NIN'},
  NIN: {reportJob: 'NINJA', moduleJob: 'NIN'},
  SAMURAI: {reportJob: 'SAMURAI', moduleJob: 'SAM'},
  SAM: {reportJob: 'SAMURAI', moduleJob: 'SAM'},
  REAPER: {reportJob: 'REAPER', moduleJob: 'RPR'},
  RPR: {reportJob: 'REAPER', moduleJob: 'RPR'},
  VIPER: {reportJob: 'VIPER', moduleJob: 'VPR'},
  VPR: {reportJob: 'VIPER', moduleJob: 'VPR'},

  BARD: {reportJob: 'BARD', moduleJob: 'BRD'},
  BRD: {reportJob: 'BARD', moduleJob: 'BRD'},
  MACHINIST: {reportJob: 'MACHINIST', moduleJob: 'MCH'},
  MCH: {reportJob: 'MACHINIST', moduleJob: 'MCH'},
  DANCER: {reportJob: 'DANCER', moduleJob: 'DNC'},
  DNC: {reportJob: 'DANCER', moduleJob: 'DNC'},

  BLACKMAGE: {reportJob: 'BLACK_MAGE', moduleJob: 'BLM'},
  BLACK_MAGE: {reportJob: 'BLACK_MAGE', moduleJob: 'BLM'},
  BLM: {reportJob: 'BLACK_MAGE', moduleJob: 'BLM'},
  SUMMONER: {reportJob: 'SUMMONER', moduleJob: 'SMN'},
  SMN: {reportJob: 'SUMMONER', moduleJob: 'SMN'},
  REDMAGE: {reportJob: 'RED_MAGE', moduleJob: 'RDM'},
  RED_MAGE: {reportJob: 'RED_MAGE', moduleJob: 'RDM'},
  RDM: {reportJob: 'RED_MAGE', moduleJob: 'RDM'},
  PICTOMANCER: {reportJob: 'PICTOMANCER', moduleJob: 'PCT'},
  PCT: {reportJob: 'PICTOMANCER', moduleJob: 'PCT'},
  BLUEMAGE: {reportJob: 'BLUE_MAGE', moduleJob: 'BLU'},
  BLUE_MAGE: {reportJob: 'BLUE_MAGE', moduleJob: 'BLU'},
  BLU: {reportJob: 'BLUE_MAGE', moduleJob: 'BLU'},

  LIMITBREAK: {reportJob: 'UNKNOWN', moduleJob: undefined},
  LIMIT_BREAK: {reportJob: 'UNKNOWN', moduleJob: undefined},
}

addJobKeyMapTitleCaseAliases(JOB_KEY_MAP)

function addJobKeyMapTitleCaseAliases(map) {
  for (const [key, value] of Object.entries(map)) {
    const titleUnderscore = key
      .split('_')
      .filter(Boolean)
      .map(part => part.charAt(0).toUpperCase() + part.slice(1).toLowerCase())
      .join('_')

    if (titleUnderscore && !map[titleUnderscore]) {
      map[titleUnderscore] = value
    }

    const titleSpace = titleUnderscore.replace(/_/g, ' ')
    if (titleSpace && !map[titleSpace]) {
      map[titleSpace] = value
    }

    const titleCompact = titleSpace.replace(/\s+/g, '')
    if (titleCompact && !map[titleCompact]) {
      map[titleCompact] = value
    }
  }
}

const serviceState = {
  ready: false,
  shuttingDown: false,
  server: null,
  sockets: new Set(),
  requestSeq: 0,
  metrics: {
    startedAt: new Date().toISOString(),
    requestsTotal: 0,
    requestsSucceeded: 0,
    requestsFailed: 0,
    requestsRejected: 0,
    requestsTimedOut: 0,
    bytesInTotal: 0,
  },
}

let inFlight = 0
let cachedModules = null
let cachedI18n = null
let threadPoolManager = null

function startEntry() {
  if (THREAD_WORKER_MODE) {
    startThreadWorkerLoop()
    return
  }

  if (!WORKER_MODE && shouldUseThreadPoolMode()) {
    startThreadPoolServer()
    return
  }

  if (!WORKER_MODE && PORT_POOL_COUNT > 1) {
    startServerPool(PORT_POOL_START, PORT_POOL_COUNT)
    return
  }

  try {
    bootstrap()
    serviceState.ready = true
    startServer()
  } catch (err) {
    logEvent('error', '服务初始化失败', formatError(err))
    process.exit(1)
  }
}

function shouldUseThreadPoolMode() {
  if (WORKER_MODE || THREAD_WORKER_MODE) {
    return false
  }
  if (EXECUTION_MODE === 'process' || EXECUTION_MODE === 'processes' || EXECUTION_MODE === 'pool') {
    return false
  }
  if (EXECUTION_MODE === 'thread' || EXECUTION_MODE === 'threads') {
    return THREAD_POOL_SIZE > 0
  }
  // Default behavior: prefer in-process worker thread pool whenever recommended size is greater than 1.
  return THREAD_POOL_SIZE > 1
}

function startThreadPoolServer() {
  threadPoolManager = new ThreadWorkerPool(THREAD_POOL_SIZE, {
    recommendFn: computeRecommendedThreadPoolSize,
    autoScaleIntervalMs: THREAD_POOL_AUTOSCALE_INTERVAL_MS,
  })
  serviceState.ready = true
  startServer()
}

function startThreadWorkerLoop() {
  if (!isMainThread || parentPort == null) {
    // Continue; worker_threads environment exposes parentPort and runs in-process.
  }

  try {
    bootstrap()
    serviceState.ready = true
  } catch (err) {
    if (parentPort) {
      parentPort.postMessage({
        type: 'fatal',
        ...formatError(err),
      })
    }
    process.exit(1)
    return
  }

  if (!parentPort) {
    process.exit(1)
    return
  }

  parentPort.postMessage({type: 'ready'})
  parentPort.on('message', async msg => {
    if (!msg || typeof msg !== 'object') {
      return
    }

    if (msg.type === 'shutdown') {
      process.exit(0)
      return
    }

    if (msg.type !== 'analyze') {
      return
    }

    try {
      const results = await withTimeout(
        handleAnalyzeLocal(msg.payload),
        ANALYZE_TIMEOUT_MS,
        `工作线程解析超时（${ANALYZE_TIMEOUT_MS}ms）`,
      )
      parentPort.postMessage({
        type: 'result',
        taskId: msg.taskId,
        ok: true,
        results,
      })
    } catch (err) {
      parentPort.postMessage({
        type: 'result',
        taskId: msg.taskId,
        ok: false,
        ...formatError(err),
      })
    }
  })
}

class ThreadWorkerPool {
  constructor(size, options = {}) {
    this.size = Math.max(1, Number(size) || 1)
    this.desiredSize = this.size
    this.queue = []
    this.inFlight = new Map()
    this.workers = []
    this.workerSeq = 0
    this.taskSeq = 0
    this.shuttingDown = false
    this.recommendFn = typeof options.recommendFn === 'function' ? options.recommendFn : null
    this.autoScaleIntervalMs = parsePositiveInt(options.autoScaleIntervalMs, THREAD_POOL_AUTOSCALE_INTERVAL_MS)
    this.autoScaleTimer = null
    this.recommendedInfo = this.normalizeRecommendation(
      this.recommendFn
        ? this.recommendFn()
        : {recommended: this.size, reason: 'thread-pool-default'},
    )
    this.sizeSource = `recommended:${this.recommendedInfo.reason}`

    for (let i = 0; i < this.size; i++) {
      this.workers.push(this.spawnWorker())
    }

    this.startAutoScaleLoop()
  }

  normalizeRecommendation(info) {
    const fallbackCpuCap = clampInt(getCpuParallelism(), 1, 64)
    const base = info && typeof info === 'object' ? info : {}
    const cpuCap = clampInt(parsePositiveInt(base.cpuCap, fallbackCpuCap), 1, 256)
    const recommended = clampInt(parsePositiveInt(base.recommended, this.desiredSize), 1, cpuCap)
    const reason = typeof base.reason === 'string' && base.reason !== ''
      ? base.reason
      : 'thread-pool-default'

    return {
      ...base,
      cpuCap,
      recommended,
      reason,
    }
  }

  startAutoScaleLoop() {
    if (!this.recommendFn || this.autoScaleIntervalMs <= 0) {
      return
    }

    this.autoScaleTimer = setInterval(() => {
      this.refreshAutoScale()
    }, this.autoScaleIntervalMs)

    if (this.autoScaleTimer && typeof this.autoScaleTimer.unref === 'function') {
      this.autoScaleTimer.unref()
    }
  }

  stopAutoScaleLoop() {
    if (this.autoScaleTimer) {
      clearInterval(this.autoScaleTimer)
      this.autoScaleTimer = null
    }
  }

  refreshAutoScale() {
    if (this.shuttingDown || !this.recommendFn) {
      return
    }

    const previousSize = this.desiredSize
    const nextInfo = this.normalizeRecommendation(this.recommendFn())

    this.recommendedInfo = nextInfo
    this.sizeSource = `recommended:${nextInfo.reason}`

    if (nextInfo.recommended === previousSize) {
      return
    }

    this.resize(nextInfo.recommended)
    logEvent('info', '线程池已自动伸缩', {
      mode: 'threads',
      previousSize,
      targetSize: this.desiredSize,
      recommendedThreadPool: this.recommendedInfo,
      autoScaleIntervalMs: this.autoScaleIntervalMs,
    })
  }

  replaceWorkerState(oldState, newState) {
    const index = this.workers.indexOf(oldState)
    if (index >= 0) {
      this.workers[index] = newState
      return
    }
    this.workers.push(newState)
  }

  removeWorkerState(state) {
    const index = this.workers.indexOf(state)
    if (index >= 0) {
      this.workers.splice(index, 1)
    }
  }

  terminateWorkerState(state) {
    if (!state || !state.worker) {
      return
    }

    try {
      state.worker.postMessage({type: 'shutdown'})
    } catch (_err) {
    }

    state.worker.terminate().catch(() => {})
  }

  retireWorkerState(state) {
    if (!state || state.retiring) {
      return
    }

    state.retiring = true
    if (!state.busy) {
      this.terminateWorkerState(state)
    }
  }

  resize(targetSize) {
    if (this.shuttingDown) {
      return
    }

    const target = clampInt(parsePositiveInt(targetSize, this.desiredSize), 1, 128)
    if (target === this.desiredSize) {
      return
    }

    this.desiredSize = target
    const activeWorkers = this.workers.filter(state => state && !state.retiring)

    if (target > activeWorkers.length) {
      const grow = target - activeWorkers.length
      for (let i = 0; i < grow; i++) {
        this.workers.push(this.spawnWorker())
      }
      this.drain()
      return
    }

    const shrink = activeWorkers.length - target
    const candidates = activeWorkers
      .slice()
      .sort((a, b) => {
        if (a.busy === b.busy) {
          return 0
        }
        return a.busy ? 1 : -1
      })

    for (let i = 0; i < shrink && i < candidates.length; i++) {
      this.retireWorkerState(candidates[i])
    }
  }

  spawnWorker() {
    this.workerSeq += 1
    const workerId = this.workerSeq
    const worker = new Worker(__filename, {
      env: {
        ...process.env,
        XIVA_THREAD_WORKER: '1',
        XIVA_WORKER_MODE: '0',
      },
    })

    const state = {
      index: workerId,
      worker,
      ready: false,
      busy: false,
      currentTaskId: null,
      retiring: false,
    }

    worker.on('message', msg => this.onWorkerMessage(state, msg))
    worker.on('error', err => {
      logEvent('error', '线程工作进程错误', {
        mode: 'threads',
        index,
        error: err && err.message ? err.message : String(err),
      })
    })
    worker.on('exit', code => this.onWorkerExit(state, code))
    return state
  }

  onWorkerMessage(state, msg) {
    if (!msg || typeof msg !== 'object') {
      return
    }

    if (msg.type === 'ready') {
      state.ready = true
      if (state.retiring) {
        this.terminateWorkerState(state)
        return
      }
      this.drain()
      return
    }

    if (msg.type === 'fatal') {
      logEvent('error', '线程工作进程初始化失败', {
        mode: 'threads',
        index: state.index,
        ...msg,
      })
      return
    }

    if (msg.type !== 'result') {
      return
    }

    const taskId = Number(msg.taskId)
    const slot = this.inFlight.get(taskId)
    if (!slot) {
      state.busy = false
      state.currentTaskId = null
      if (state.retiring) {
        this.terminateWorkerState(state)
        return
      }
      this.drain()
      return
    }

    this.inFlight.delete(taskId)
    state.busy = false
    state.currentTaskId = null

    if (msg.ok) {
      slot.resolve(msg.results)
    } else {
      slot.reject(new Error(msg.error || '线程解析失败'))
    }

    if (state.retiring) {
      this.terminateWorkerState(state)
      return
    }

    this.drain()
  }

  onWorkerExit(state, code) {
    const taskId = state.currentTaskId
    if (taskId != null) {
      const slot = this.inFlight.get(taskId)
      if (slot) {
        this.inFlight.delete(taskId)
        if (!this.shuttingDown && !state.retiring) {
          this.queue.unshift({
            id: slot.id,
            payload: slot.payload,
            resolve: slot.resolve,
            reject: slot.reject,
          })
        } else {
          slot.reject(new Error('线程池正在关闭'))
        }
      }
    }

    if (this.shuttingDown || state.retiring) {
      this.removeWorkerState(state)
      this.drain()
      return
    }

    logEvent('warn', '线程工作进程退出，准备重启', {
      mode: 'threads',
      index: state.index,
      code,
    })
    const replacement = this.spawnWorker()
    this.replaceWorkerState(state, replacement)
    this.drain()
  }

  runTask(payload) {
    if (this.shuttingDown) {
      return Promise.reject(new Error('线程池正在关闭'))
    }

    return new Promise((resolve, reject) => {
      this.taskSeq += 1
      this.queue.push({
        id: this.taskSeq,
        payload,
        resolve,
        reject,
      })
      this.drain()
    })
  }

  drain() {
    if (this.shuttingDown) {
      return
    }

    for (const state of this.workers) {
      if (!state || state.retiring || !state.ready || state.busy) {
        continue
      }
      const task = this.queue.shift()
      if (!task) {
        return
      }
      state.busy = true
      state.currentTaskId = task.id
      this.inFlight.set(task.id, task)
      state.worker.postMessage({
        type: 'analyze',
        taskId: task.id,
        payload: task.payload,
      })
    }
  }

  async shutdown(timeoutMs) {
    if (this.shuttingDown) {
      return
    }
    this.shuttingDown = true
    this.stopAutoScaleLoop()

    while (this.queue.length > 0) {
      const task = this.queue.shift()
      task.reject(new Error('线程池正在关闭'))
    }

    for (const [taskId, task] of this.inFlight.entries()) {
      this.inFlight.delete(taskId)
      task.reject(new Error('线程池正在关闭'))
    }

    const terminate = Promise.all(this.workers.map(async state => {
      if (!state || !state.worker) {
        return
      }
      try {
        state.worker.postMessage({type: 'shutdown'})
      } catch (_err) {
      }
      try {
        await state.worker.terminate()
      } catch (_err) {
      }
    }))

    if (!timeoutMs || timeoutMs <= 0) {
      await terminate
      return
    }

    await Promise.race([
      terminate,
      new Promise(resolve => setTimeout(resolve, timeoutMs)),
    ])
  }

  getSizingInfo() {
    return {
      threadPoolSize: this.desiredSize,
      threadPoolSizeSource: this.sizeSource,
      recommendedThreadPool: this.recommendedInfo,
      autoScaleIntervalMs: this.autoScaleIntervalMs,
    }
  }

  stats() {
    let ready = 0
    let busy = 0
    let retiring = 0
    let active = 0
    for (const state of this.workers) {
      if (!state) {
        continue
      }
      if (state.retiring) {
        retiring += 1
        continue
      }

      active += 1
      if (state.ready) {
        ready += 1
      }
      if (state.busy) {
        busy += 1
      }
    }
    return {
      size: active,
      desiredSize: this.desiredSize,
      totalWorkers: this.workers.length,
      retiring,
      ready,
      busy,
      queued: this.queue.length,
      inFlight: this.inFlight.size,
      autoScaleIntervalMs: this.autoScaleIntervalMs,
    }
  }
}

startEntry()

function startServerPool(portStart, count) {
  const workers = []

  logEvent('info', '启动 xivanalysis 多进程服务池', {
    mode: 'pool',
    portStart,
    count,
  })

  for (let i = 0; i < count; i++) {
    const port = portStart + i
    const worker = fork(__filename, [], {
      env: {
        ...process.env,
        PORT: String(port),
        XIVA_WORKER_MODE: '1',
        XIVA_PORT_COUNT: '1',
      },
      stdio: 'inherit',
    })

    workers.push({worker, port})

    worker.on('exit', (code, signal) => {
      logEvent('warn', 'xivanalysis 工作进程退出', {
        mode: 'pool',
        port,
        pid: worker.pid,
        code,
        signal,
      })
    })
  }

  const shutdownPool = signal => {
    logEvent('warn', '正在关闭 xivanalysis 服务池', {
      mode: 'pool',
      signal,
      count: workers.length,
    })

    for (const item of workers) {
      const child = item.worker
      if (!child.killed) {
        child.kill('SIGTERM')
      }
    }

    setTimeout(() => {
      for (const item of workers) {
        const child = item.worker
        if (!child.killed) {
          child.kill('SIGKILL')
        }
      }
      process.exit(0)
    }, SHUTDOWN_TIMEOUT_MS)
  }

  process.on('SIGINT', () => shutdownPool('SIGINT'))
  process.on('SIGTERM', () => shutdownPool('SIGTERM'))
}

function logEvent(level, message, extra = {}) {
  const inputLevel = LEVEL_WEIGHT[level] != null ? level : 'info'
  if (LEVEL_WEIGHT[inputLevel] < (LEVEL_WEIGHT[LOG_LEVEL] ?? LEVEL_WEIGHT.info)) {
    return
  }

  const payload = {
    timestamp: new Date().toISOString(),
    level: inputLevel,
    message,
    ...extra,
  }
  process.stdout.write(`${JSON.stringify(payload)}\n`)
}

function bootstrap() {
  ensureSymbolMetadata()

  if (!process.env.LINGUI_CONFIG) {
    process.env.LINGUI_CONFIG = path.join(xivaRoot, 'lingui.config.ts')
  }
  if (!process.env.NODE_ENV) {
    process.env.NODE_ENV = 'test'
  }

  require(path.join(xivaRoot, 'node_modules', '@babel', 'register'))({
    cwd: xivaRoot,
    rootMode: 'upward',
    extensions: ['.ts', '.tsx', '.js', '.jsx'],
    cache: false,
  })

  registerAssetStubs()
  registerGlobals()
  loadModulesOnce()
  patchInjection()
  patchMissingDependencies()
}

function ensureSymbolMetadata() {
  if (typeof Symbol.metadata === 'undefined') {
    // Babel decorators runtime stores metadata on Symbol.metadata (or Symbol.for fallback).
    // Ensure both writer and reader use the same symbol in Node runtime.
    Symbol.metadata = Symbol.for('Symbol.metadata')
  }
}

function loadModulesOnce() {
  if (cachedModules) {
    return cachedModules
  }
  process.env.NODE_PATH = process.env.NODE_PATH
    ? `${process.env.NODE_PATH}${path.delimiter}${xivaSrc}`
    : xivaSrc
  Module._initPaths()
  registerAliases(xivaSrc)

  const encounters = require(path.join(xivaSrc, 'data/ENCOUNTERS'))
  const editions = require(path.join(xivaSrc, 'data/EDITIONS'))
  const available = require(path.join(xivaSrc, 'parser/AVAILABLE_MODULES'))
  const parser = require(path.join(xivaSrc, 'parser/core/Parser'))
  const adapter = require(path.join(xivaSrc, 'reportSources/legacyFflogs/eventAdapter/adapter'))
  const report = require(path.join(xivaSrc, 'report'))
    
  cachedModules = {
    getEncounterKey: encounters.getEncounterKey,
    GameEdition: editions.GameEdition,
    AVAILABLE_MODULES: available.AVAILABLE_MODULES,
    Parser: parser.Parser,
    adaptEvents: adapter.adaptEvents,
    Team: report.Team,
  }
  return cachedModules
}

function patchInjection() {
  let Parser
  let Analyser
  let toposort
  try {
    Parser = require(path.join(xivaSrc, 'parser/core/Parser')).Parser
    Analyser = require(path.join(xivaSrc, 'parser/core/Analyser')).Analyser
    toposort = require(path.join(xivaRoot, 'node_modules', 'toposort'))
  } catch (_err) {
    return
  }

  if (!Parser || Parser.prototype._xivaInjectionPatched) {
    return
  }

  Parser.prototype.configure = async function () {
    const constructors = await this.loadModuleConstructors()
    const nodes = Object.keys(constructors)
    const edges = []
    nodes.forEach(mod => constructors[mod].dependencies.forEach(dep => {
      if (OPTIONAL_DEPENDENCY_HANDLES.has(dep.handle)) {
        return
      }
      edges.push([mod, dep.handle])
    }))

    try {
      this.executionOrder = toposort.array(nodes, edges).reverse()
    } catch (error) {
      if (!(error instanceof Error) || !error.message.includes('unknown node')) {
        throw error
      }

      const nodeSet = new Set(nodes)
      const unknown = edges
        .filter(([_source, target]) => !nodeSet.has(target))
        .map(([source, target]) => `${source}→${target}`)

      logEvent('warn', '依赖图存在未注册模块，已裁剪相关边', {
        unknownEdges: unknown,
      })

      const filteredEdges = edges.filter(([_source, target]) => nodeSet.has(target))
      this.executionOrder = toposort.array(nodes, filteredEdges).reverse()
    }

    const loadedHandles = []
    const failedHandles = new Set()

    this.executionOrder.forEach(handle => {
      const ctor = constructors[handle]
      const missingDeps = ctor.dependencies
        .map(dep => dep.handle)
        .filter(depHandle => !OPTIONAL_DEPENDENCY_HANDLES.has(depHandle))
        .filter(depHandle => this.container[depHandle] == null || failedHandles.has(depHandle))

      if (missingDeps.length > 0) {
        failedHandles.add(handle)
        logEvent('warn', '模块缺少依赖，已跳过', {
          module: handle,
          missingDependencies: missingDeps,
        })
        return
      }

      let injectable
      try {
        injectable = new ctor(this)
        this.container[handle] = injectable
        reinjectDependencies(injectable, this.container)

        if (injectable instanceof Analyser) {
          injectable.initialise()
        } else {
          throw new Error(`未处理的可注入类型，无法初始化：${handle}`)
        }
      } catch (error) {
        failedHandles.add(handle)
        delete this.container[handle]
        logEvent('warn', '模块构造失败，已跳过', {
          module: handle,
          error: error instanceof Error ? error.message : String(error),
        })
        return
      }

      loadedHandles.push(handle)
    })

    this.executionOrder = loadedHandles
  }

  Parser.prototype._xivaInjectionPatched = true
}

function reinjectDependencies(instance, container) {
  const ctor = instance && instance.constructor
  const deps = ctor && ctor.dependencies ? ctor.dependencies : []

  for (const dependency of deps) {
    const mapped = typeof dependency === 'string'
      ? {handle: dependency, prop: dependency}
      : dependency
    if (OPTIONAL_DEPENDENCY_HANDLES.has(mapped.handle) && container[mapped.handle] == null) {
      continue
    }
    instance[mapped.prop] = container[mapped.handle]
  }

  for (const [key, value] of Object.entries(container)) {
    const proto = Object.getPrototypeOf(instance)
    const descriptor = Object.getOwnPropertyDescriptor(proto, key)
    if (descriptor && typeof descriptor.get === 'function') {
      continue
    }
    if (instance[key] === undefined) {
      instance[key] = value
    }
  }

  const aliasMap = {
    globalCooldown: 'gcd',
  }
  for (const [prop, handle] of Object.entries(aliasMap)) {
    if (instance[prop] == null && container[handle]) {
      instance[prop] = container[handle]
    }
  }
}

function startServer() {
  const server = http.createServer((req, res) => {
    const requestId = resolveRequestId(req)
    res.setHeader('X-Request-Id', requestId)

    const url = new URL(req.url || '/', 'http://localhost')
    const pathname = url.pathname
    const method = (req.method || 'GET').toUpperCase()

    if (method === 'GET' && pathname === '/health') {
      const mode = threadPoolManager ? 'threads' : (WORKER_MODE ? 'process-worker' : 'single')
      const threadPoolConfigInfo = getThreadPoolConfigInfo()
      const concurrencyInfo = getCurrentConcurrencyInfo(threadPoolConfigInfo)
      sendJson(res, 200, {
        status: 'ok',
        statusZh: '正常',
        ready: serviceState.ready,
        shuttingDown: serviceState.shuttingDown,
        inFlight,
        threadPoolSize: threadPoolConfigInfo.threadPoolSize,
        threadPoolSizeSource: threadPoolConfigInfo.threadPoolSizeSource,
        recommendedThreadPool: threadPoolConfigInfo.recommendedThreadPool,
        threadPoolAutoScaleIntervalMs: threadPoolConfigInfo.autoScaleIntervalMs,
        maxConcurrency: concurrencyInfo.maxConcurrency,
        maxConcurrencySource: concurrencyInfo.maxConcurrencySource,
        recommendedConcurrency: concurrencyInfo.recommendedConcurrency,
        mode,
        threadPool: threadPoolManager ? threadPoolManager.stats() : undefined,
        ...buildConcurrencyCompatFields(mode, threadPoolConfigInfo, concurrencyInfo),
      })
      return
    }

    if (method === 'GET' && pathname === '/ready') {
      if (serviceState.ready && !serviceState.shuttingDown) {
        sendJson(res, 200, {status: 'ready', statusZh: '就绪'})
      } else {
        sendJson(res, 503, {
          status: 'not_ready',
          statusZh: '未就绪',
          ready: serviceState.ready,
          shuttingDown: serviceState.shuttingDown,
        })
      }
      return
    }

    if (method === 'GET' && pathname === '/metrics') {
      const mode = threadPoolManager ? 'threads' : (WORKER_MODE ? 'process-worker' : 'single')
      const threadPoolConfigInfo = getThreadPoolConfigInfo()
      const concurrencyInfo = getCurrentConcurrencyInfo(threadPoolConfigInfo)
      sendJson(res, 200, {
        ...serviceState.metrics,
        inFlight,
        threadPoolSize: threadPoolConfigInfo.threadPoolSize,
        threadPoolSizeSource: threadPoolConfigInfo.threadPoolSizeSource,
        recommendedThreadPool: threadPoolConfigInfo.recommendedThreadPool,
        threadPoolAutoScaleIntervalMs: threadPoolConfigInfo.autoScaleIntervalMs,
        maxConcurrency: concurrencyInfo.maxConcurrency,
        maxConcurrencySource: concurrencyInfo.maxConcurrencySource,
        recommendedConcurrency: concurrencyInfo.recommendedConcurrency,
        mode,
        threadPool: threadPoolManager ? threadPoolManager.stats() : undefined,
        uptimeMs: Math.max(0, Date.now() - Date.parse(serviceState.metrics.startedAt)),
        ...buildConcurrencyCompatFields(mode, threadPoolConfigInfo, concurrencyInfo),
      })
      return
    }

    if (method !== 'POST' || pathname !== '/analyze') {
      sendJson(res, 404, {error: '未找到接口'})
      return
    }

    if (serviceState.shuttingDown || !serviceState.ready) {
      serviceState.metrics.requestsRejected += 1
      sendJson(res, 503, {error: '服务暂不可用', requestId})
      return
    }

    if (API_KEY !== '') {
      const headerApiKey = req.headers['x-api-key']
      const suppliedKey = typeof headerApiKey === 'string' ? headerApiKey : ''
      if (suppliedKey !== API_KEY) {
        serviceState.metrics.requestsRejected += 1
        sendJson(res, 401, {error: '未授权访问', requestId})
        return
      }
    }

    if (!isJsonContentType(req.headers['content-type'])) {
      serviceState.metrics.requestsRejected += 1
      sendJson(res, 415, {error: 'content-type 必须为 application/json', requestId})
      return
    }

    const runtimeConcurrencyInfo = getCurrentConcurrencyInfo()
    if (inFlight >= runtimeConcurrencyInfo.maxConcurrency) {
      serviceState.metrics.requestsRejected += 1
      sendJson(res, 429, {error: '服务繁忙，请稍后重试', requestId})
      return
    }

    const startedAt = Date.now()
    const chunks = []
    let receivedBytes = 0
    let bodyRejected = false

    req.on('data', chunk => {
      if (bodyRejected) {
        return
      }

      receivedBytes += chunk.length
      if (receivedBytes > MAX_BODY_BYTES) {
        bodyRejected = true
        serviceState.metrics.requestsRejected += 1
        sendJson(res, 413, {error: '请求体过大', requestId})
        req.destroy()
        return
      }

      chunks.push(chunk)
    })

    req.on('aborted', () => {
      logEvent('warn', '请求已中断', {requestId})
    })

    req.on('error', err => {
      serviceState.metrics.requestsFailed += 1
      logEvent('error', '请求流错误', {
        requestId,
        error: err && err.message ? err.message : String(err),
      })
      sendJson(res, 500, {error: `请求错误：${err.message}`, requestId})
    })

    req.on('end', async () => {
      if (bodyRejected) {
        return
      }

      const parsed = parseJSONSafe(Buffer.concat(chunks))
      if (!parsed.ok) {
        serviceState.metrics.requestsRejected += 1
        sendJson(res, 400, {error: `JSON 无效：${parsed.error}`, requestId})
        return
      }

      inFlight += 1
      serviceState.metrics.requestsTotal += 1
      serviceState.metrics.bytesInTotal += receivedBytes

      try {
        const payload = parsed.value || {}
        const results = await withTimeout(
          handleAnalyze(payload),
          ANALYZE_TIMEOUT_MS,
          `解析超时（${ANALYZE_TIMEOUT_MS}ms）`,
        )

        serviceState.metrics.requestsSucceeded += 1
        sendJson(res, 200, {
          requestId,
          durationMs: Date.now() - startedAt,
          results,
        })

        logEvent('info', '请求处理完成', {
          requestId,
          status: 200,
          durationMs: Date.now() - startedAt,
          inFlight,
        })
      } catch (err) {
        const isTimeout = err instanceof TimeoutError
        if (isTimeout) {
          serviceState.metrics.requestsTimedOut += 1
        }
        serviceState.metrics.requestsFailed += 1

        const status = isTimeout ? 504 : 500
        const payload = {
          requestId,
          ...formatError(err),
        }
        sendJson(res, status, payload)

        logEvent('error', '请求处理失败', {
          requestId,
          status,
          durationMs: Date.now() - startedAt,
          ...payload,
        })
      } finally {
        inFlight -= 1
      }
    })
  })

  serviceState.server = server
  server.requestTimeout = ANALYZE_TIMEOUT_MS + 5 * 1000
  server.keepAliveTimeout = 5 * 1000

  server.on('connection', socket => {
    serviceState.sockets.add(socket)
    socket.on('close', () => {
      serviceState.sockets.delete(socket)
    })
    if (serviceState.shuttingDown) {
      socket.destroy()
    }
  })

  server.on('error', err => {
    logEvent('error', '服务错误', {
      error: err && err.message ? err.message : String(err),
    })
  })

  registerShutdownHandlers(server)

  server.listen(PORT, HOST, () => {
    const mode = threadPoolManager ? 'threads' : (WORKER_MODE ? 'process-worker' : 'single')
    const threadPoolConfigInfo = getThreadPoolConfigInfo()
    const concurrencyInfo = getCurrentConcurrencyInfo(threadPoolConfigInfo)
    logEvent('info', 'xivanalysis 服务已启动', {
      host: HOST,
      port: PORT,
      mode,
      threadPoolSize: threadPoolConfigInfo.threadPoolSize,
      threadPoolSizeSource: threadPoolConfigInfo.threadPoolSizeSource,
      recommendedThreadPool: threadPoolConfigInfo.recommendedThreadPool,
      threadPoolAutoScaleIntervalMs: threadPoolConfigInfo.autoScaleIntervalMs,
      threadPool: threadPoolManager ? threadPoolManager.stats() : undefined,
      maxBodyBytes: MAX_BODY_BYTES,
      maxConcurrency: concurrencyInfo.maxConcurrency,
      maxConcurrencySource: concurrencyInfo.maxConcurrencySource,
      recommendedConcurrency: concurrencyInfo.recommendedConcurrency,
      analyzeTimeoutMs: ANALYZE_TIMEOUT_MS,
      shutdownTimeoutMs: SHUTDOWN_TIMEOUT_MS,
      apiKeyEnabled: API_KEY !== '',
      metricsPath: '/metrics',
      readyPath: '/ready',
      ...buildConcurrencyCompatFields(mode, threadPoolConfigInfo, concurrencyInfo),
    })
  })
}

class TimeoutError extends Error {
  constructor(message) {
    super(message)
    this.name = 'TimeoutError'
  }
}

function withTimeout(promise, timeoutMs, message) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new TimeoutError(message))
    }, timeoutMs)

    Promise.resolve(promise)
      .then(value => {
        clearTimeout(timer)
        resolve(value)
      })
      .catch(err => {
        clearTimeout(timer)
        reject(err)
      })
  })
}

function registerShutdownHandlers(server) {
  const shutdown = signal => {
    if (serviceState.shuttingDown) {
      return
    }

    serviceState.shuttingDown = true
    serviceState.ready = false

    logEvent('warn', '开始关闭服务', {
      signal,
      inFlight,
      openSockets: serviceState.sockets.size,
    })

    const forceTimer = setTimeout(() => {
      for (const socket of serviceState.sockets) {
        socket.destroy()
      }
      logEvent('error', '关闭超时，强制退出', {
        signal,
        inFlight,
        openSockets: serviceState.sockets.size,
      })
      process.exit(1)
    }, SHUTDOWN_TIMEOUT_MS)

    server.close(err => {
      clearTimeout(forceTimer)
      if (err) {
        logEvent('error', '服务关闭失败', {
          signal,
          error: err && err.message ? err.message : String(err),
        })
        process.exit(1)
        return
      }

      const finalizeExit = async () => {
        if (threadPoolManager) {
          await threadPoolManager.shutdown(Math.max(0, SHUTDOWN_TIMEOUT_MS - 500))
        }
        logEvent('info', '服务已完成关闭', {signal})
        process.exit(0)
      }

      finalizeExit().catch(err => {
        logEvent('error', '线程池关闭失败', {
          signal,
          error: err && err.message ? err.message : String(err),
        })
        process.exit(1)
      })
    })
  }

  process.on('SIGINT', () => shutdown('SIGINT'))
  process.on('SIGTERM', () => shutdown('SIGTERM'))
}

function resolveRequestId(req) {
  const raw = req.headers['x-request-id']
  if (typeof raw === 'string' && raw.trim() !== '') {
    return raw.trim().slice(0, 128)
  }

  serviceState.requestSeq += 1
  return `req-${Date.now()}-${serviceState.requestSeq}`
}

function isJsonContentType(contentType) {
  if (typeof contentType !== 'string') {
    return false
  }
  return contentType.toLowerCase().includes('application/json')
}

function sendJson(res, statusCode, payload) {
  if (res.writableEnded) {
    return
  }

  const body = JSON.stringify(payload)
  res.writeHead(statusCode, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': Buffer.byteLength(body),
  })
  res.end(body)
}

function formatError(err) {
  const message = err && err.message ? err.message : String(err)
  const includeStack = process.env.DEBUG_ERRORS === '1'
  return includeStack
    ? {error: message, stack: err && err.stack ? String(err.stack) : undefined}
    : {error: message}
}

async function handleAnalyze(payload) {
  if (threadPoolManager) {
    return threadPoolManager.runTask(payload)
  }
  return handleAnalyzeLocal(payload)
}

async function handleAnalyzeLocal(payload) {
  const requests = Array.isArray(payload.requests) ? payload.requests : [payload]
  if (requests.length === 0) {
    throw new Error('requests 必须是非空数组')
  }

  const results = []
  for (let i = 0; i < requests.length; i++) {
    const req = requests[i]
    const result = await analyzeRequest(req, i)
    results.push(result)
  }
  return results
}

async function analyzeRequest(request, index) {
  const mods = loadModulesOnce()
  const normalized = normalizeRequest(request)
  if (normalized.error) {
    return {
      index,
      status: 'error',
      error: normalized.error,
    }
  }

  const {code, report, fight, fightId, events} = normalized
  const actorIdsInFight = collectActorIds(events)
  const pull = buildPull(mods, report, fight, actorIdsInFight)
  const xvReport = buildReport(mods, code, report, [pull])
  const firstEvent = report.startTime + fight.startTime
  const adapted = mods.adaptEvents(xvReport, pull, events, firstEvent)
  ensurePullActorsForAdaptedEvents(mods, pull, adapted)

  const resultsByActor = []
  for (const actor of pull.actors) {
    if (actor.team !== mods.Team.FRIEND || !actor.playerControlled) {
      continue
    }

    // AVAILABLE_MODULES.JOBS uses full job keys (e.g. WHITE_MAGE), not short aliases (e.g. WHM).
    // Use actor.job to ensure job modules are merged and checklist contains full checks.
    const meta = buildMeta(mods, pull.encounter.key, actor.job)
    const parser = new mods.Parser({
      meta,
      report: xvReport,
      pull,
      actor,
    })

    preseedCoreModules(parser)
    await parser.configure()
    parser.parseEvents({events: adapted})
    const parsedResults = parser.generateResults()

    const moduleErrors = parser._moduleErrors || {}
    const moduleErrorKeys = Object.keys(moduleErrors)
    if (process.env.DEBUG_MODULE_ERRORS === '1' && moduleErrorKeys.length > 0) {
      const summarizedErrors = {}
      for (const key of moduleErrorKeys) {
        const err = moduleErrors[key]
        summarizedErrors[key] = err && err.message ? err.message : String(err)
      }
      logEvent('warn', '检测到解析模块错误', {
        reportCode: code,
        fightId,
        actorId: actor.id,
        actorName: actor.name,
        actorJob: actor.job,
        moduleErrors: summarizedErrors,
      })
    }

    resultsByActor.push({
      actorId: actor.id,
      name: actor.name,
      job: actor.job,
      modules: serializeResults(parsedResults, parser),
    })
  }

  return {
    status: 'ok',
    reportCode: code,
    fightId,
    fightName: fight.name,
    startTime: report.startTime + fight.startTime,
    endTime: report.startTime + fight.endTime,
    actors: resultsByActor,
  }
}

function normalizeRequest(request) {
  if (!request || typeof request !== 'object') {
    return {error: 'request 必须是对象'}
  }
  const code = request.code || request.reportCode || request.report_code || 'unknown'
  const report = request.report || request.metadata || request.reportData
  const events = normalizeEvents(request.events || request.eventData || request.event_data)
  const fightId = Number(request.fightId || request.fight_id || request.fight || 0)
  const fight = request.fight

  if (!report || typeof report !== 'object') {
    return {error: '缺少 report 元数据'}
  }
  const normalizedReport = normalizeReport(report, request.actors || request.friendlies || request.masterData)
  if (normalizedReport.error) {
    return {error: normalizedReport.error}
  }
  if (!events || !Array.isArray(events)) {
    return {error: '缺少 events 数组'}
  }

  let selectedFight = fight
  if (!selectedFight && Array.isArray(normalizedReport.fights)) {
    selectedFight = normalizedReport.fights.find(f => Number(f.id) === fightId)
  }
  if (!selectedFight) {
    return {error: '在 report 中未找到 fight，请提供 fight 或 fightId'}
  }

  return {
    code,
    report: normalizedReport,
    fight: selectedFight,
    fightId: Number(selectedFight.id),
    events,
  }
}

function normalizeReport(report, actorsFallback) {
  const missing = []
  if (report.startTime == null) missing.push('startTime')
  if (report.endTime == null) missing.push('endTime')
  if (!report.title) missing.push('title')
  if (!Array.isArray(report.fights)) missing.push('fights')
  if (missing.length > 0) {
    return {error: `report 缺少字段：${missing.join(', ')}`}
  }

  let masterData = report.masterData
  if (!masterData || !Array.isArray(masterData.actors)) {
    const actors = normalizeActors(actorsFallback || report.actors || report.friendlies)
    if (!actors) {
      return {error: 'report.masterData.actors 缺失，请提供 actors'}
    }
    masterData = {actors}
  }

  return {
    ...report,
    masterData,
  }
}

function normalizeActors(input) {
  if (!input) return null
  if (Array.isArray(input)) return input
  if (input && Array.isArray(input.actors)) return input.actors
  return null
}

function normalizeEvents(raw) {
  if (!raw) return null
  if (Array.isArray(raw)) return raw
  if (raw && Array.isArray(raw.events)) return raw.events
  if (raw && raw.events && Array.isArray(raw.events.events)) return raw.events.events
  return null
}

function parseJSONSafe(buf) {
  try {
    return {ok: true, value: JSON.parse(buf)}
  } catch (err) {
    return {ok: false, error: err.message}
  }
}

function buildReport(mods, code, data, pulls) {
  return {
    timestamp: data.startTime,
    edition: mods.GameEdition.GLOBAL,
    name: data.title,
    pulls,
    meta: {
      source: 'legacyFflogs',
      code,
      end: data.endTime,
      fights: data.fights,
      friendlies: [],
      enemies: [],
    },
  }
}

function buildPull(mods, report, fight, actorIdsInFight) {
  const encounterKey = fight.encounterID != null
    ? mods.getEncounterKey('legacyFflogs', String(fight.encounterID))
    : undefined

  const actors = report.masterData.actors
    .filter(a => !actorIdsInFight || actorIdsInFight.has(toActorId(a.id)))
    .map(a => buildActor(mods, a))

  return {
    id: fight.id.toString(),
    timestamp: report.startTime + fight.startTime,
    duration: fight.endTime - fight.startTime,
    progress: fight.kill ? 100 : fight.fightPercentage != null ? 100 - fight.fightPercentage / 100 : undefined,
    encounter: {
      key: encounterKey,
      name: fight.name,
      duty: {
        id: fight.gameZone ? fight.gameZone.id : 0,
        name: fight.gameZone ? fight.gameZone.name : 'Unknown',
      },
    },
    actors,
  }
}

function collectActorIds(events) {
  const ids = new Set()
  for (const event of events) {
    const sourceId = event.sourceID ?? event.sourceId
    const targetId = event.targetID ?? event.targetId
    if (sourceId != null) ids.add(toActorId(sourceId))
    if (targetId != null) ids.add(toActorId(targetId))
    if (Array.isArray(event.targets)) {
      for (const target of event.targets) {
        const tid = target.targetID ?? target.targetId
        if (tid != null) ids.add(toActorId(tid))
      }
    }
  }
  return ids
}

function ensurePullActorsForAdaptedEvents(mods, pull, adaptedEvents) {
  const existingActorIds = new Set(pull.actors.map(actor => actor.id))
  const adaptedActorIds = collectAdaptedActorIds(adaptedEvents)

  for (const actorId of adaptedActorIds) {
    if (existingActorIds.has(actorId)) {
      continue
    }
    pull.actors.push(buildPlaceholderActor(mods, actorId))
    existingActorIds.add(actorId)
  }
}

function collectAdaptedActorIds(events) {
  const ids = new Set()
  for (const event of events) {
    if (event.actor != null) {
      ids.add(toActorId(event.actor))
    }
    if (event.source != null) {
      ids.add(toActorId(event.source))
    }
    if (event.target != null) {
      ids.add(toActorId(event.target))
    }
    if (Array.isArray(event.targets)) {
      for (const target of event.targets) {
        if (target && target.target != null) {
          ids.add(toActorId(target.target))
        }
      }
    }
  }
  return ids
}

function buildPlaceholderActor(mods, actorId) {
  return {
    kind: actorId,
    name: actorId.startsWith('unknown') ? '未知' : `角色 ${actorId}`,
    team: mods.Team.UNKNOWN != null ? mods.Team.UNKNOWN : mods.Team.FOE,
    playerControlled: false,
    job: 'UNKNOWN',
    moduleJob: undefined,
    id: actorId,
  }
}

function toActorId(id) {
  return String(id)
}

function buildActor(mods, actor) {
  const job = normalizeJobKey(actor.subType)
  const actorType = typeof actor.type === 'string' ? actor.type.toLowerCase() : ''
  const isPlayer = actorType === 'player'
  const isFriendly = inferFriendlyActor(actor, actorType)
  return {
    kind: actor.gameID != null ? String(actor.gameID) : String(actor.id),
    name: actor.name,
    team: isFriendly ? mods.Team.FRIEND : mods.Team.FOE,
    playerControlled: isPlayer && job.reportJob !== 'UNKNOWN',
    job: job.reportJob,
    moduleJob: job.moduleJob,
    id: toActorId(actor.id),
  }
}

function inferFriendlyActor(actor, actorType) {
  if (actor && typeof actor.friendly === 'boolean') {
    return actor.friendly
  }
  if (actor && typeof actor.hostile === 'boolean') {
    return !actor.hostile
  }

  const type = typeof actorType === 'string' ? actorType : ''
  if (type === 'player' || type === 'pet' || type === 'companion') {
    return true
  }
  if (type === 'npc' || type === 'enemy' || type === 'boss') {
    return false
  }

  return false
}

function normalizeJobKey(subType) {
  if (!subType || typeof subType !== 'string') {
    return {reportJob: 'UNKNOWN', moduleJob: undefined}
  }

  const raw = subType.trim()
  if (JOB_KEY_MAP[raw]) {
    return JOB_KEY_MAP[raw]
  }

  const rawUnderscore = raw.replace(/[-\s]+/g, '_')
  if (JOB_KEY_MAP[rawUnderscore]) {
    return JOB_KEY_MAP[rawUnderscore]
  }

  const normalized = subType.trim().toUpperCase().replace(/[-\s]+/g, '_')
  if (JOB_KEY_MAP[normalized]) {
    return JOB_KEY_MAP[normalized]
  }

  const compact = normalized.replace(/_/g, '')
  if (JOB_KEY_MAP[compact]) {
    return JOB_KEY_MAP[compact]
  }

  return {reportJob: normalized, moduleJob: undefined}
}

function buildMeta(mods, encounterKey, job) {
  let meta = mods.AVAILABLE_MODULES.CORE
  if (encounterKey && mods.AVAILABLE_MODULES.BOSSES[encounterKey]) {
    meta = meta.merge(mods.AVAILABLE_MODULES.BOSSES[encounterKey])
  }
  if (job && mods.AVAILABLE_MODULES.JOBS[job]) {
    meta = meta.merge(mods.AVAILABLE_MODULES.JOBS[job])
  }
  return meta
}

function serializeResults(results, parser) {
  const React = require(path.join(xivaRoot, 'node_modules', 'react'))
  const {renderToStaticMarkup} = require(path.join(xivaRoot, 'node_modules', 'react-dom', 'server'))
  const {I18nProvider} = require(path.join(xivaRoot, 'node_modules', '@lingui', 'react'))
  const {Provider: DbTooltipProvider} = require(path.join(xivaSrc, 'components/ui/DbLink'))
  const linguiI18n = getLinguiI18n()
  const includeHtml = OUTPUT_MODE === 'html' || OUTPUT_MODE === 'both'

  return results.map(result => {
    const moduleInstance = parser && parser.container ? parser.container[result.handle] : undefined
    const metrics = extractModuleMetrics(result.handle, moduleInstance)

    let html
    if (includeHtml) {
      try {
        const withTooltipProvider = React.createElement(
          DbTooltipProvider,
          null,
          result.markup,
        )
        const wrapped = React.createElement(
          I18nProvider,
          {i18n: linguiI18n, defaultComponent: LinguiDefaultWrapper},
          withTooltipProvider,
        )
        html = renderToStaticMarkup(wrapped)
      } catch (err) {
        html = '[渲染错误]'
        if (process.env.DEBUG_RENDER_ERRORS === '1') {
          logEvent('warn', '模块渲染失败', {
            module: result.handle,
            error: err && err.message ? err.message : String(err),
          })
        }
      }
    }

    const name = resolveResultName(result, parser)
    const output = {
      handle: result.handle,
      name,
      mode: result.mode,
      order: result.order,
      metrics,
    }

    if (includeHtml) {
      output.html = html
    }

    return output
  })
}

function resolveResultName(result, parser) {
  if (result && result.name && typeof result.name.id === 'string') {
    return translateMessageId(result.name.id, typeof result.name.message === 'string' ? result.name.message : undefined)
  }
  const fallback = reactNodeToText(result ? result.name : undefined, parser)
  return fallback || (result && result.handle ? result.handle : '')
}

function extractModuleMetrics(handle, moduleInstance) {
  if (!moduleInstance || typeof moduleInstance !== 'object') {
    return {}
  }

  const normalizedHandle = typeof handle === 'string' ? handle.toLowerCase() : ''
  const metrics = {}

  if (normalizedHandle === 'checklist') {
    Object.assign(metrics, extractChecklistMetrics(moduleInstance))
  } else if (normalizedHandle === 'suggestions') {
    Object.assign(metrics, extractSuggestionsMetrics(moduleInstance))
  } else if (normalizedHandle === 'statistics') {
    Object.assign(metrics, extractStatisticsMetrics(moduleInstance))
  } else if (normalizedHandle === 'defensives' || normalizedHandle === 'utilities') {
    Object.assign(metrics, extractUtilitiesMetrics(moduleInstance))
  } else if (normalizedHandle === 'meikyo' || normalizedHandle === 'tincture') {
    Object.assign(metrics, extractActionWindowMetrics(moduleInstance))
  } else if (normalizedHandle === 'sen') {
    Object.assign(metrics, extractSenMetrics(moduleInstance))
  } else if (normalizedHandle === 'higanbana') {
    Object.assign(metrics, extractDotMetrics(moduleInstance))
  } else if (normalizedHandle === 'aoeusages') {
    Object.assign(metrics, extractAoEUsageMetrics(moduleInstance))
  }

  if (typeof moduleInstance.getTotalDelay === 'function') {
    try {
      metrics.totalDelayMs = Number(moduleInstance.getTotalDelay()) || 0
    } catch (_err) {
      // noop
    }
  }

  if (typeof moduleInstance.getIssueData === 'function') {
    try {
      const issues = moduleInstance.getIssueData()
      if (Array.isArray(issues)) {
        metrics.issueCount = issues.length
        if (issues.length > 0) {
          metrics.issues = issues.map(issue => ({
            timestamp: issue && issue.timestamp != null ? Number(issue.timestamp) : undefined,
            startMs: issue && issue.start != null ? Number(issue.start) : undefined,
            stopMs: issue && issue.stop != null ? Number(issue.stop) : undefined,
            delayMs: issue && issue.delay != null ? Number(issue.delay) : undefined,
          }))
        }
      }
    } catch (_err) {
      // noop
    }
  }

  if (typeof moduleInstance.gcdsCounted === 'number') {
    metrics.gcdCount = moduleInstance.gcdsCounted
  }

  if (Object.keys(metrics).length === 0) {
    Object.assign(metrics, extractGenericModuleMetrics(handle, moduleInstance))
  }

  return metrics
}

function extractGenericModuleMetrics(handle, moduleInstance) {
  const metrics = {
    extractor: 'generic',
    handle: typeof handle === 'string' ? handle : '',
  }

  const ctorName = moduleInstance && moduleInstance.constructor && moduleInstance.constructor.name
  if (typeof ctorName === 'string' && ctorName.length > 0) {
    metrics.moduleClass = ctorName
  }

  const counters = {}

  if (Array.isArray(moduleInstance && moduleInstance.trackedActions)) {
    counters.trackedActionCount = moduleInstance.trackedActions.length
  }

  if (Array.isArray(moduleInstance && moduleInstance.trackedStatuses)) {
    counters.trackedStatusCount = moduleInstance.trackedStatuses.length
  }

  if (Array.isArray(moduleInstance && moduleInstance.senStateWindows)) {
    counters.windowCount = moduleInstance.senStateWindows.length
  }

  const historyEntries = moduleInstance && moduleInstance.history && Array.isArray(moduleInstance.history.entries)
    ? moduleInstance.history.entries
    : undefined
  if (Array.isArray(historyEntries)) {
    counters.windowCount = historyEntries.length
  }

  if (moduleInstance && moduleInstance.badUsages && typeof moduleInstance.badUsages.size === 'number') {
    counters.badUsageKeyCount = moduleInstance.badUsages.size
  }

  if (moduleInstance && moduleInstance.statusApplications && typeof moduleInstance.statusApplications.size === 'number') {
    counters.statusApplicationTargetCount = moduleInstance.statusApplications.size
  }

  if (Object.keys(counters).length > 0) {
    metrics.counters = counters
  }

  if (!metrics.moduleClass) {
    metrics.moduleClass = 'UnknownModule'
  }

  return metrics
}

function extractActionWindowMetrics(moduleInstance) {
  const parser = moduleInstance && moduleInstance.parser ? moduleInstance.parser : undefined
  const pullStart = parser && parser.pull && typeof parser.pull.timestamp === 'number'
    ? parser.pull.timestamp
    : 0

  const historyEntries = Array.isArray(moduleInstance && moduleInstance.history && moduleInstance.history.entries)
    ? moduleInstance.history.entries
    : []

  const windows = historyEntries.map(entry => {
    const start = entry && typeof entry.start === 'number' ? entry.start : undefined
    const end = entry && typeof entry.end === 'number' ? entry.end : undefined
    const actions = Array.isArray(entry && entry.data) ? entry.data : []
    const actionCounts = countActionsById(actions)

    return {
      startMs: typeof start === 'number' ? Math.max(0, start - pullStart) : undefined,
      endMs: typeof end === 'number' ? Math.max(0, end - pullStart) : undefined,
      durationMs: typeof start === 'number' && typeof end === 'number' ? Math.max(0, end - start) : undefined,
      actionCount: actions.length,
      actions: actionCounts,
    }
  })

  const totalTrackedActions = windows.reduce((sum, window) => sum + (window.actionCount || 0), 0)

  return {
    windowCount: windows.length,
    totalTrackedActions,
    averageActionsPerWindow: windows.length > 0 ? totalTrackedActions / windows.length : 0,
    windows,
  }
}

function extractSenMetrics(moduleInstance) {
  const parser = moduleInstance && moduleInstance.parser ? moduleInstance.parser : undefined
  const pullStart = parser && parser.pull && typeof parser.pull.timestamp === 'number'
    ? parser.pull.timestamp
    : 0

  const windows = Array.isArray(moduleInstance && moduleInstance.senStateWindows)
    ? moduleInstance.senStateWindows
    : []

  const serializedWindows = windows.map(window => {
    const rotation = Array.isArray(window && window.rotation) ? window.rotation : []
    const actionCounts = countActionsById(rotation)

    return {
      startMs: typeof window.start === 'number' ? Math.max(0, window.start - pullStart) : undefined,
      endMs: typeof window.end === 'number' ? Math.max(0, window.end - pullStart) : undefined,
      isNonStandard: !!(window && window.isNonStandard),
      hasHagakure: !!(window && window.hasHagakure),
      hasOverwrite: !!(window && window.hasOverwrite),
      isDeath: !!(window && window.isDeath),
      totalSenGenerated: numberFromUnknown(window ? window.totalSenGenerated : undefined),
      wastedSens: numberFromUnknown(window ? window.wastedSens : undefined),
      currentSens: numberFromUnknown(window ? window.currentSens : undefined),
      kenkiGained: numberFromUnknown(window ? window.kenkiGained : undefined),
      reason: reactNodeToText(window && window.senCode ? window.senCode.message : undefined, parser),
      rotationCount: rotation.length,
      actions: actionCounts,
    }
  })

  return {
    windowCount: serializedWindows.length,
    nonStandardWindowCount: numberFromUnknown(moduleInstance && moduleInstance.nonStandardCount),
    hagakureWindowCount: numberFromUnknown(moduleInstance && moduleInstance.hagakureCount),
    wastedSens: numberFromUnknown(moduleInstance && moduleInstance.wasted),
    windows: serializedWindows,
  }
}

function extractDotMetrics(moduleInstance) {
  const parser = moduleInstance && moduleInstance.parser ? moduleInstance.parser : undefined
  const trackedStatuses = Array.isArray(moduleInstance && moduleInstance.trackedStatuses)
    ? moduleInstance.trackedStatuses
    : []

  const statuses = trackedStatuses.map(statusId => {
    const status = safeCall(() => moduleInstance.data.getStatus(statusId), undefined)
    const name = reactNodeToText(status ? status.name : undefined, parser)
    const uptimePercent = safeCall(() => moduleInstance.getUptimePercent(statusId), undefined)
    const clippingAmount = safeCall(() => moduleInstance.getClippingAmount(statusId), undefined)

    const statusApplications = moduleInstance && moduleInstance.statusApplications
    const tracked = statusApplications && typeof statusApplications.get === 'function'
      ? statusApplications.get(statusId)
      : undefined

    let targetCount = 0
    let applicationCount = 0
    if (tracked && typeof tracked.size === 'number') {
      targetCount = tracked.size
      if (typeof tracked.forEach === 'function') {
        tracked.forEach(targetTracking => {
          const timestamps = Array.isArray(targetTracking && targetTracking.applicationTimestamps)
            ? targetTracking.applicationTimestamps
            : []
          applicationCount += timestamps.length
        })
      }
    }

    return {
      id: Number(statusId),
      name: name || `Status ${statusId}`,
      uptimePercent: numberFromUnknown(uptimePercent),
      clippingPerMinuteMs: numberFromUnknown(clippingAmount),
      targetCount,
      applicationCount,
    }
  })

  const primaryUptime = statuses.length > 0 ? statuses[0].uptimePercent : undefined

  return {
    trackedStatusCount: statuses.length,
    uptimePercent: primaryUptime,
    statuses,
  }
}

function extractAoEUsageMetrics(moduleInstance) {
  const parser = moduleInstance && moduleInstance.parser ? moduleInstance.parser : undefined
  const trackedActions = Array.isArray(moduleInstance && moduleInstance.trackedActions)
    ? moduleInstance.trackedActions
    : []

  const badUsageMap = moduleInstance && moduleInstance.badUsages && typeof moduleInstance.badUsages.get === 'function'
    ? moduleInstance.badUsages
    : undefined

  const actions = trackedActions.map(tracked => {
    const aoeAction = tracked && tracked.aoeAction ? tracked.aoeAction : undefined
    const aoeId = aoeAction && aoeAction.id != null ? Number(aoeAction.id) : undefined
    const badUsages = badUsageMap && aoeId != null
      ? Number(badUsageMap.get(aoeId) || 0)
      : 0

    const stActions = Array.isArray(tracked && tracked.stActions) ? tracked.stActions : []

    return {
      aoeActionId: aoeId,
      aoeActionName: reactNodeToText(aoeAction ? aoeAction.name : undefined, parser) || (aoeId != null ? `Action ${aoeId}` : 'Action'),
      minTargets: tracked && tracked.minTargets != null ? Number(tracked.minTargets) : undefined,
      badUsages,
      stAlternatives: stActions.map(action => ({
        id: action && action.id != null ? Number(action.id) : undefined,
        name: reactNodeToText(action ? action.name : undefined, parser) || (action && action.id != null ? `Action ${action.id}` : 'Action'),
      })),
    }
  })

  const totalBadUsages = actions.reduce((sum, action) => sum + (action.badUsages || 0), 0)

  return {
    trackedActionCount: actions.length,
    badActionCount: actions.filter(action => (action.badUsages || 0) > 0).length,
    totalBadUsages,
    actions,
  }
}

function countActionsById(events) {
  const actionCountMap = new Map()

  for (const event of events) {
    const actionId = event && event.action != null ? Number(event.action) : undefined
    if (!Number.isFinite(actionId)) {
      continue
    }
    actionCountMap.set(actionId, (actionCountMap.get(actionId) || 0) + 1)
  }

  return Array.from(actionCountMap.entries())
    .sort((a, b) => b[1] - a[1])
    .map(([id, count]) => ({id, count}))
}

function extractUtilitiesMetrics(moduleInstance) {
  const parser = moduleInstance && moduleInstance.parser ? moduleInstance.parser : undefined
  const trackedActions = Array.isArray(moduleInstance.trackedActions) ? moduleInstance.trackedActions : []

  const actions = trackedActions.map(action => {
    const uses = safeCall(() => moduleInstance.getUsageCount(action), undefined)
    const maxUses = safeCall(() => moduleInstance.getMaxUses(action), undefined)
    const actionName = reactNodeToText(action && action.name ? action.name : undefined, parser)

    return {
      id: action && action.id != null ? Number(action.id) : undefined,
      name: actionName || (action && action.id != null ? `Action ${action.id}` : 'Action'),
      uses: typeof uses === 'number' ? uses : undefined,
      maxUses: typeof maxUses === 'number' ? maxUses : undefined,
      usageRate: typeof uses === 'number' && typeof maxUses === 'number' && maxUses > 0
        ? (uses / maxUses) * 100
        : undefined,
    }
  })

  const totalUses = actions.reduce((sum, action) => sum + (typeof action.uses === 'number' ? action.uses : 0), 0)
  const totalMaxUses = actions.reduce((sum, action) => sum + (typeof action.maxUses === 'number' ? action.maxUses : 0), 0)

  return {
    trackedActionCount: actions.length,
    totalUses,
    totalMaxUses: totalMaxUses > 0 ? totalMaxUses : undefined,
    overallUsageRate: totalMaxUses > 0 ? (totalUses / totalMaxUses) * 100 : undefined,
    actions,
  }
}

function safeCall(fn, fallback) {
  try {
    return fn()
  } catch (_err) {
    return fallback
  }
}

function extractChecklistMetrics(moduleInstance) {
  const parser = moduleInstance && moduleInstance.parser ? moduleInstance.parser : undefined
  const rules = Array.isArray(moduleInstance.rules) ? moduleInstance.rules : []
  const serializedRules = rules.map(rule => {
    const requirements = Array.isArray(rule.requirements) ? rule.requirements : []
    const serializedRequirements = requirements.map(req => {
      const percent = req && req.percent != null ? Number(req.percent) : 0
      const target = req && req.target != null ? Number(req.target) : 0
      const value = req && req.value != null ? Number(req.value) : undefined
      return {
        name: reactNodeToText(req ? req.name : undefined, parser),
        percent,
        value,
        target,
        weight: req && req.weight != null ? Number(req.weight) : 1,
        passed: percent >= target,
        display: formatRequirementDisplay(value, target, percent),
      }
    })

    const percent = rule && rule.percent != null ? Number(rule.percent) : 0
    const target = rule && rule.target != null ? Number(rule.target) : 0
    return {
      name: reactNodeToText(rule ? rule.name : undefined, parser),
      description: reactNodeToText(rule ? rule.description : undefined, parser),
      percent,
      target,
      passed: !!(rule && rule.passed),
      requirements: serializedRequirements,
    }
  })

  const passedRules = serializedRules.filter(rule => rule.passed).length
  const averagePercent = serializedRules.length > 0
    ? serializedRules.reduce((sum, rule) => sum + rule.percent, 0) / serializedRules.length
    : 0

  const grouped = serializedRules.map((rule, ruleIndex) => ({
    main: rule.name || `Rule ${ruleIndex + 1}`,
    percent: rule.percent,
    target: rule.target,
    passed: rule.passed,
    display: formatPercent(rule.percent),
    internal: rule.requirements.map((requirement, reqIndex) => ({
      name: requirement.name || `Requirement ${reqIndex + 1}`,
      value: buildGroupedInternalDisplay(requirement),
      passed: requirement.passed,
      percent: requirement.percent,
      target: requirement.target,
    })),
  }))

  const groupedLines = []
  for (const item of grouped) {
    groupedLines.push(`主 ${item.main}`)
    for (const inner of item.internal) {
      groupedLines.push(`内部 ${inner.name}: ${inner.value}`)
    }
  }

  return {
    ruleCount: serializedRules.length,
    passedRules,
    failedRules: Math.max(0, serializedRules.length - passedRules),
    averagePercent,
    rules: serializedRules,
    grouped,
    groupedLines,
  }
}

function extractSuggestionsMetrics(moduleInstance) {
  const suggestions = Array.isArray(moduleInstance._suggestions) ? moduleInstance._suggestions : []
  const serialized = suggestions.map(suggestion => {
    const severityRaw = suggestion && suggestion.severity != null ? Number(suggestion.severity) : undefined
    return {
      severity: Number.isFinite(severityRaw) ? severityRaw : undefined,
      severityRaw: severityRaw != null ? String(severityRaw) : undefined,
      value: suggestion && suggestion.value != null ? Number(suggestion.value) : undefined,
      content: reactNodeToText(suggestion ? suggestion.content : undefined, moduleInstance && moduleInstance.parser),
      why: reactNodeToText(suggestion ? suggestion.why : undefined, moduleInstance && moduleInstance.parser),
    }
  })

  const severityCount = {}
  for (const item of serialized) {
    const key = item.severity != null
      ? String(item.severity)
      : item.severityRaw || 'unknown'
    severityCount[key] = (severityCount[key] || 0) + 1
  }

  return {
    suggestionCount: serialized.length,
    severityCount,
    suggestions: serialized,
  }
}

function extractStatisticsMetrics(moduleInstance) {
  const stats = Array.isArray(moduleInstance.statistics) ? moduleInstance.statistics : []

  const serialized = stats.map(stat => {
    const one = {
      type: stat && stat.constructor ? stat.constructor.name : 'UnknownStatistic',
      width: stat && stat.width != null ? Number(stat.width) : undefined,
      height: stat && stat.height != null ? Number(stat.height) : undefined,
      displayOrder: stat && stat.statsDisplayOrder != null ? Number(stat.statsDisplayOrder) : undefined,
    }

    if (Array.isArray(stat && stat.data)) {
      const values = stat.data
        .map(item => (item && item.value != null ? Number(item.value) : 0))
        .filter(value => !Number.isNaN(value))
      one.series = values
      one.total = values.reduce((sum, value) => sum + value, 0)
    }

    if (Array.isArray(stat && stat.rows)) {
      one.rowCount = stat.rows.length
    }

    const numericValue = numberFromUnknown(stat ? stat.value : undefined)
    if (numericValue != null) {
      one.value = numericValue
    }

    one.title = reactNodeToText(stat ? stat.title : undefined, moduleInstance && moduleInstance.parser)
    return one
  })

  return {
    statisticCount: serialized.length,
    statistics: serialized,
  }
}

function numberFromUnknown(value) {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value
  }
  if (typeof value === 'string') {
    const matched = value.replace(/,/g, '').match(/-?\d+(\.\d+)?/)
    if (matched) {
      return Number(matched[0])
    }
  }
  return null
}

function formatPercent(value) {
  const number = Number(value)
  if (!Number.isFinite(number)) {
    return ''
  }
  return `${number.toFixed(2)}%`
}

function formatValue(value) {
  const number = Number(value)
  if (!Number.isFinite(number)) {
    return ''
  }
  if (Math.abs(number - Math.round(number)) < 1e-9) {
    return String(Math.round(number))
  }
  return number.toFixed(2)
}

function formatRequirementDisplay(value, target, percent) {
  if (value != null && target != null && Number.isFinite(Number(target)) && Number(target) > 0) {
    return `${formatValue(value)} / ${formatValue(target)} (${formatPercent(percent)})`
  }
  return formatPercent(percent)
}

function buildGroupedInternalDisplay(requirement) {
  if (!requirement) {
    return ''
  }
  if (requirement.value != null && requirement.target != null && Number.isFinite(Number(requirement.target)) && Number(requirement.target) > 0) {
    return `${formatValue(requirement.value)} / ${formatValue(requirement.target)} (${formatPercent(requirement.percent)})`
  }
  return formatPercent(requirement.percent)
}

function prettifyKey(key) {
  if (!key || typeof key !== 'string') {
    return ''
  }
  return key
    .toLowerCase()
    .split('_')
    .filter(Boolean)
    .map(part => part.charAt(0).toUpperCase() + part.slice(1))
    .join(' ')
}

function resolveActionName(actionKey, parser) {
  const actions = parser && parser.container && parser.container.data && parser.container.data.actions
  const action = actions && actionKey ? actions[actionKey] : undefined
  const fromData = action && action.name ? reactNodeToText(action.name, parser) : ''
  return fromData || prettifyKey(actionKey)
}

function resolveStatusName(statusKey, parser) {
  const statuses = parser && parser.container && parser.container.data && parser.container.data.statuses
  const status = statuses && statusKey ? statuses[statusKey] : undefined
  const fromData = status && status.name ? reactNodeToText(status.name, parser) : ''
  return fromData || prettifyKey(statusKey)
}

function decodeHtmlEntities(text) {
  if (!text) {
    return ''
  }
  return text
    .replace(/&nbsp;/g, ' ')
    .replace(/&amp;/g, '&')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&#x27;/g, "'")
    .replace(/&quot;/g, '"')
}

function htmlToPlainText(html) {
  if (!html) {
    return ''
  }
  const noTags = String(html).replace(/<[^>]+>/g, ' ')
  return decodeHtmlEntities(noTags).replace(/\s+/g, ' ').trim()
}

function translateMessageId(id, fallback) {
  if (!id || typeof id !== 'string') {
    return fallback || ''
  }
  try {
    const i18n = getLinguiI18n()
    const translated = i18n._({id, message: fallback || id})
    if (typeof translated === 'string' && translated.trim() !== '') {
      return translated
    }
  } catch (_err) {
    // noop
  }
  return fallback || id
}

function renderNodeToText(node, parser) {
  try {
    const React = require(path.join(xivaRoot, 'node_modules', 'react'))
    const {renderToStaticMarkup} = require(path.join(xivaRoot, 'node_modules', 'react-dom', 'server'))
    const {I18nProvider} = require(path.join(xivaRoot, 'node_modules', '@lingui', 'react'))
    const {Provider: DbTooltipProvider} = require(path.join(xivaSrc, 'components/ui/DbLink'))
    const {DataContextProvider} = require(path.join(xivaSrc, 'components/DataContext'))
    const linguiI18n = getLinguiI18n()

    let wrapped = node
    if (parser && parser.patch) {
      wrapped = React.createElement(DataContextProvider, {patch: parser.patch}, wrapped)
    }
    wrapped = React.createElement(DbTooltipProvider, null, wrapped)
    wrapped = React.createElement(I18nProvider, {i18n: linguiI18n, defaultComponent: LinguiDefaultWrapper}, wrapped)

    const html = renderToStaticMarkup(wrapped)
    return htmlToPlainText(html)
  } catch (_err) {
    return ''
  }
}

function reactNodeToText(node, parser) {
  if (node == null || typeof node === 'boolean') {
    return ''
  }
  if (typeof node === 'string' || typeof node === 'number') {
    return String(node)
  }
  if (Array.isArray(node)) {
    return node.map(item => reactNodeToText(item, parser)).join('').trim()
  }
  if (typeof node === 'object' && typeof node.id === 'string') {
    return translateMessageId(node.id, typeof node.message === 'string' ? node.message : undefined)
  }
  if (typeof node === 'object' && node.props) {
    if (typeof node.props.action === 'string') {
      return resolveActionName(node.props.action, parser)
    }
    if (typeof node.props.status === 'string') {
      return resolveStatusName(node.props.status, parser)
    }
    if (typeof node.props.item === 'string') {
      return resolveActionName(node.props.item, parser)
    }
    if (typeof node.props.id === 'string') {
      const renderedWithChildren = renderNodeToText(node, parser)
      if (renderedWithChildren) {
        return renderedWithChildren
      }
      return translateMessageId(node.props.id, typeof node.props.message === 'string' ? node.props.message : undefined)
    }
    if (typeof node.props.message === 'string') {
      return node.props.message
    }
    const fromChildren = reactNodeToText(node.props.children, parser)
    if (fromChildren) {
      return fromChildren
    }
    const rendered = renderNodeToText(node, parser)
    if (rendered) {
      return rendered
    }
    return ''
  }
  return ''
}

function LinguiDefaultWrapper(props) {
  return props && props.translation != null ? props.translation : null
}

function normalizeLinguiCatalog(rawCatalog) {
  if (!rawCatalog || typeof rawCatalog !== 'object') {
    return {}
  }
  if (rawCatalog.messages && typeof rawCatalog.messages === 'object') {
    return rawCatalog.messages
  }
  return rawCatalog
}

function getLinguiI18n() {
  if (cachedI18n) {
    return cachedI18n
  }

  const {i18n} = require(path.join(xivaRoot, 'node_modules', '@lingui', 'core'))
  const enCatalog = require(path.join(xivaRoot, 'locale', 'en', 'messages.json'))
  i18n.load('en', normalizeLinguiCatalog(enCatalog))

  let activeLocale = 'en'
  if (OUTPUT_LOCALE !== 'en') {
    try {
      const targetCatalog = require(path.join(xivaRoot, 'locale', OUTPUT_LOCALE, 'messages.json'))
      i18n.load(OUTPUT_LOCALE, normalizeLinguiCatalog(targetCatalog))
      activeLocale = OUTPUT_LOCALE
    } catch (_err) {
      // Keep English fallback if target locale is unavailable.
      activeLocale = 'en'
    }
  }

  i18n.activate(activeLocale)
  cachedI18n = i18n
  return cachedI18n
}

function patchMissingDependencies() {
  const statusTimelinePath = path.join(xivaSrc, 'parser/core/modules/StatusTimeline')
  let StatusTimeline
  try {
    StatusTimeline = require(statusTimelinePath).StatusTimeline
  } catch (_err) {
    return
  }

  if (!StatusTimeline || StatusTimeline.prototype._xivaDepsPatched) {
    return
  }

  const originalInit = StatusTimeline.prototype.initialise
  StatusTimeline.prototype.initialise = function () {
    if (!this.actionTimeline) {
      this.actionTimeline = this.parser.container.actionTimeline
    }
    if (!this.actors) {
      this.actors = this.parser.container.actors
    }
    if (!this.data) {
      this.data = this.parser.container.data
    }
    if (!this.data || !this.data.actions) {
      console.warn('[xiva] StatusTimeline missing data/actions; skipping initialisation')
      return
    }
    return originalInit.call(this)
  }
  StatusTimeline.prototype._xivaDepsPatched = true

  const resourceGraphsPath = path.join(xivaSrc, 'parser/core/modules/ResourceGraphs')
  let ResourceGraphs
  try {
    ResourceGraphs = require(resourceGraphsPath).ResourceGraphs
  } catch (_err) {
    return
  }

  if (!ResourceGraphs || ResourceGraphs.prototype._xivaDepsPatched) {
    return
  }

  const originalAddDataGroup = ResourceGraphs.prototype.addDataGroup
  ResourceGraphs.prototype.addDataGroup = function (...args) {
    if (!this.timeline && this.parser && this.parser.container) {
      this.timeline = this.parser.container.timeline
    }
    return originalAddDataGroup.apply(this, args)
  }
  ResourceGraphs.prototype._xivaDepsPatched = true

  const speedAdjustmentsPath = path.join(xivaSrc, 'parser/core/modules/SpeedAdjustments')
  let SpeedAdjustments
  try {
    SpeedAdjustments = require(speedAdjustmentsPath).SpeedAdjustments
  } catch (_err) {
    return
  }

  if (!SpeedAdjustments || SpeedAdjustments.prototype._xivaFallbackPatched) {
    return
  }

  const originalGetAdjustedDuration = SpeedAdjustments.prototype.getAdjustedDuration
  SpeedAdjustments.prototype.getAdjustedDuration = function (opts) {
    try {
      return originalGetAdjustedDuration.call(this, opts)
    } catch (_err) {
      return opts?.duration ?? 2500
    }
  }

  const originalIsAdjustmentEstimated = SpeedAdjustments.prototype.isAdjustmentEstimated
  SpeedAdjustments.prototype.isAdjustmentEstimated = function (opts) {
    try {
      return originalIsAdjustmentEstimated.call(this, opts)
    } catch (_err) {
      return true
    }
  }

  SpeedAdjustments.prototype._xivaFallbackPatched = true
}

function preseedCoreModules(parser) {
  if (!parser.container) {
    parser.container = {}
  }
  try {
    if (!parser.container.timeline) {
      const Timeline = require(path.join(xivaSrc, 'parser/core/modules/Timeline')).Timeline
      parser.container.timeline = new Timeline(parser)
    }
  } catch (err) {
    console.warn(`[xiva] failed to preseed Timeline: ${err.message || err}`)
  }
  try {
    if (!parser.container.data) {
      const Data = require(path.join(xivaSrc, 'parser/core/modules/Data')).Data
      parser.container.data = new Data(parser)
    }
  } catch (err) {
    console.warn(`[xiva] failed to preseed Data: ${err.message || err}`)
  }
  try {
    if (!parser.container.suggestions) {
      const Suggestions = require(path.join(xivaSrc, 'parser/core/modules/Suggestions')).Suggestions
      parser.container.suggestions = new Suggestions(parser)
    }
  } catch (err) {
    console.warn(`[xiva] failed to preseed Suggestions: ${err.message || err}`)
  }
  try {
    if (!parser.container.resourceGraphs) {
      const ResourceGraphs = require(path.join(xivaSrc, 'parser/core/modules/ResourceGraphs')).ResourceGraphs
      parser.container.resourceGraphs = new ResourceGraphs(parser)
    }
  } catch (err) {
    console.warn(`[xiva] failed to preseed ResourceGraphs: ${err.message || err}`)
  }
}

function registerAliases(srcPath) {
  const aliasRoots = new Set([
    'parser',
    'components',
    'utilities',
    'reportSources',
    'report',
    'errors',
    'event',
    'env',
    'store',
  ])

  const originalResolve = Module._resolveFilename
  Module._resolveFilename = function (request, parent, isMain, options) {
    const root = request.split('/')[0]
    if (aliasRoots.has(root)) {
      const basePath = path.join(srcPath, request)
      const resolved = resolveWithExtensions(basePath)
      if (resolved) {
        return originalResolve.call(this, resolved, parent, isMain, options)
      }
    }
    return originalResolve.call(this, request, parent, isMain, options)
  }
}

function resolveWithExtensions(basePath) {
  const candidates = [
    basePath,
    `${basePath}.ts`,
    `${basePath}.tsx`,
    `${basePath}.js`,
    path.join(basePath, 'index.ts'),
    path.join(basePath, 'index.tsx'),
    path.join(basePath, 'index.js'),
  ]

  for (const candidate of candidates) {
    if (fs.existsSync(candidate)) {
      return candidate
    }
  }

  return undefined
}

function registerAssetStubs() {
  const emptyExport = module => {
    module.exports = ''
  }
  const emptyObject = module => {
    module.exports = {}
  }
  const assetExts = ['.jpg', '.jpeg', '.png', '.gif', '.svg', '.webp']
  for (const ext of assetExts) {
    require.extensions[ext] = emptyExport
  }
  const styleExts = ['.css', '.scss', '.sass', '.less']
  for (const ext of styleExts) {
    require.extensions[ext] = emptyObject
  }
}

function registerGlobals() {
  if (typeof global.localStorage === 'undefined') {
    global.localStorage = {
      getItem() {
        return null
      },
      setItem() {},
      removeItem() {},
      clear() {},
    }
  }
  if (typeof global.sessionStorage === 'undefined') {
    global.sessionStorage = {
      getItem() {
        return null
      },
      setItem() {},
      removeItem() {},
      clear() {},
    }
  }
  if (typeof global.window === 'undefined') {
    global.window = {
      location: {
        reload() {},
      },
      Date,
      performance: {
        now() {
          return Date.now()
        },
      },
    }
  }
  if (typeof global.WheelEvent === 'undefined') {
    global.WheelEvent = function WheelEvent() {}
    global.window.WheelEvent = global.WheelEvent
  }
  if (typeof global.requestAnimationFrame === 'undefined') {
    global.requestAnimationFrame = callback => setTimeout(() => callback(Date.now()), 0)
  }
  if (typeof global.cancelAnimationFrame === 'undefined') {
    global.cancelAnimationFrame = id => clearTimeout(id)
  }
  if (typeof global.window.requestAnimationFrame === 'undefined') {
    global.window.requestAnimationFrame = global.requestAnimationFrame
  }
  if (typeof global.window.cancelAnimationFrame === 'undefined') {
    global.window.cancelAnimationFrame = global.cancelAnimationFrame
  }
}
