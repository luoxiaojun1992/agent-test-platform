import type { RecentJob } from './types'

const tokenKey = 'atp.api-token'
const recentJobsKey = 'atp.recent-generation-jobs'

export function loadToken(): string {
  return sessionStorage.getItem(tokenKey) ?? ''
}

export function saveToken(token: string): void {
  if (token) sessionStorage.setItem(tokenKey, token)
  else sessionStorage.removeItem(tokenKey)
}

export function loadRecentJobs(): RecentJob[] {
  try {
    const value = localStorage.getItem(recentJobsKey)
    if (!value) return []
    const jobs = JSON.parse(value) as RecentJob[]
    return Array.isArray(jobs)
      ? jobs.filter((job) => typeof job.id === 'string' && typeof job.createdAt === 'string').slice(0, 12)
      : []
  } catch {
    return []
  }
}

export function rememberJob(job: RecentJob, current: RecentJob[]): RecentJob[] {
  const next = [job, ...current.filter((item) => item.id !== job.id)].slice(0, 12)
  localStorage.setItem(recentJobsKey, JSON.stringify(next))
  return next
}