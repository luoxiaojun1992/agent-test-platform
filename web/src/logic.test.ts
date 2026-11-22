import { describe, expect, it } from 'vitest'
import { jobProgress } from './progress'
import { clamp, formatDate, shortID, titleCase, toggleValue } from './utils'
import { loadRecentJobs, loadToken, rememberJob, saveToken } from './storage'

describe('utils', () => {
  it('toggles values in and out of a list', () => {
    expect(toggleValue(['a'], 'b')).toEqual(['a', 'b'])
    expect(toggleValue(['a', 'b'], 'a')).toEqual(['b'])
  })

  it('clamps and rounds, and treats non-finite numbers as the minimum', () => {
    expect(clamp(5.6, 1, 10)).toBe(6)
    expect(clamp(99, 1, 10)).toBe(10)
    expect(clamp(-4, 1, 10)).toBe(1)
    expect(clamp(Number.NaN, 2, 10)).toBe(2)
  })

  it('formats identifiers and dates', () => {
    expect(titleCase('AWAITING_REVIEW')).toBe('Awaiting Review')
    expect(shortID('01HZZZZZZZZZZZZZZZZZZZZZZ1')).toBe('01HZZZ...ZZZ1')
    expect(shortID('short')).toBe('short')
    expect(formatDate('not a date')).toBe('not a date')
    expect(formatDate('2026-11-19T10:00:00Z')).not.toBe('2026-11-19T10:00:00Z')
  })
})

describe('storage', () => {
  it('keeps the API token in the session only', () => {
    expect(loadToken()).toBe('')
    saveToken('secret')
    expect(loadToken()).toBe('secret')
    expect(localStorage.length).toBe(0)
    saveToken('')
    expect(loadToken()).toBe('')
  })

  it('remembers at most 12 recent jobs, newest first, without duplicates', () => {
    let jobs = loadRecentJobs()
    for (let index = 0; index < 15; index += 1) jobs = rememberJob({ id: `job-${index}`, createdAt: '2026-01-01T00:00:00Z' }, jobs)
    jobs = rememberJob({ id: 'job-3', createdAt: '2026-02-01T00:00:00Z' }, jobs)
    expect(jobs).toHaveLength(12)
    expect(jobs[0].id).toBe('job-3')
    expect(jobs.filter((job) => job.id === 'job-3')).toHaveLength(1)
    expect(loadRecentJobs()).toEqual(jobs)
  })

  it('ignores corrupt stored jobs', () => {
    localStorage.setItem('atp.recent-generation-jobs', '{not json')
    expect(loadRecentJobs()).toEqual([])
    localStorage.setItem('atp.recent-generation-jobs', JSON.stringify([{ id: 1 }, { id: 'ok', createdAt: 'x' }]))
    expect(loadRecentJobs()).toEqual([{ id: 'ok', createdAt: 'x' }])
  })
})

describe('jobProgress', () => {
  const base = { job_kind: 'SCENARIO' as const, require_manual_review: false }

  it('marks earlier stages complete and the current one active', () => {
    const steps = jobProgress({ ...base, status: 'GENERATING_SCENARIO' })
    expect(steps.map((step) => step.state)).toEqual(['complete', 'complete', 'active', 'pending', 'pending', 'pending'])
  })

  it('hides the review stage unless manual review is required', () => {
    expect(jobProgress({ ...base, status: 'PENDING' }).some((step) => step.status === 'AWAITING_REVIEW')).toBe(false)
    expect(jobProgress({ ...base, require_manual_review: true, status: 'PENDING' }).some((step) => step.status === 'AWAITING_REVIEW')).toBe(true)
  })

  it('shows a repairing job on the validating stage it repairs', () => {
    const steps = jobProgress({ ...base, job_kind: 'DATASET', status: 'REPAIRING_DATASET' })
    const active = steps.find((step) => step.state === 'active')
    expect(active).toMatchObject({ status: 'VALIDATING_DATASET', repairing: true })
  })

  it('completes every stage of a finished job', () => {
    expect(jobProgress({ ...base, job_kind: 'TEST_CASES', status: 'COMPLETED' }).every((step) => step.state === 'complete')).toBe(true)
  })
})
