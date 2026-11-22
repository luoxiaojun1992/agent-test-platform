import { useQuery } from '@tanstack/react-query'
import { Archive, LoaderCircle } from 'lucide-react'
import { ApiError, getGenerationPackage } from './api'
import { ErrorBanner } from './ui'

export function PackagePanel({ token, jobId }: { token: string; jobId: string }) {
  const query = useQuery({
    queryKey: ['generation-job-package', jobId, token],
    queryFn: () => getGenerationPackage(token, jobId),
    retry: false,
  })

  return (
    <section className="workspace-panel package-panel" aria-labelledby="package-heading">
      <div className="panel-heading">
        <div>
          <span className="panel-kicker">Generation package</span>
          <h2 id="package-heading">Published package</h2>
        </div>
        <Archive aria-hidden="true" size={22} />
      </div>

      {query.isPending && (
        <div className="draft-loading"><LoaderCircle className="spin" aria-hidden="true" size={18} /> Loading package</div>
      )}
      {query.error instanceof ApiError && query.error.status === 404 && (
        <p className="draft-empty">This job did not publish a package. A package exists only for a completed generation pipeline.</p>
      )}
      {query.error && !(query.error instanceof ApiError && query.error.status === 404) && <ErrorBanner error={query.error} />}

      {query.data && (
        <>
          <dl className="package-facts">
            <div><dt>Scenario revision</dt><dd><code>{query.data.scenario_revision_id}</code></dd></div>
            <div><dt>Dataset revision</dt><dd><code>{query.data.dataset_revision_id}</code></dd></div>
            <div><dt>Manifest SHA-256</dt><dd><code>{query.data.manifest_sha256}</code></dd></div>
          </dl>
          <h4 className="draft-subhead">Test cases ({query.data.test_case_revision_ids.length})</h4>
          <ol className="package-cases">
            {query.data.test_case_revision_ids.map((id) => (
              <li key={id}><code>{id}</code></li>
            ))}
          </ol>
          <details className="raw-json">
            <summary>Generation manifest</summary>
            <pre>{JSON.stringify(query.data.manifest, null, 2)}</pre>
          </details>
        </>
      )}
    </section>
  )
}
