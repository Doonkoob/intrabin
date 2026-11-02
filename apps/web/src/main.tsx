import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router'
import './index.css'
import { PrefsProvider, SessionProvider, usePrefs, useSession } from './app'
import { Layout } from './components/Layout'
import { ErrorBanner, Spinner } from './components/ui'
import { LoginPage, SetupPage, useSetupStatus } from './pages/Auth'
import { OverviewPage } from './pages/Overview'
import { MonitorsPage } from './pages/Monitors'
import { MonitorFormPage } from './pages/MonitorForm'
import { MonitorDetailPage } from './pages/MonitorDetail'
import { GroupsPage } from './pages/Groups'
import { IncidentDetailPage, IncidentsPage } from './pages/Incidents'
import { MaintenancePage } from './pages/Maintenance'
import { ClusterPage, FollowerStatusPage } from './pages/Cluster'
import { AuditPage } from './pages/Audit'
import { AutomationPage, RunDetailPage, WorkflowDetailPage, WorkflowFormPage } from './pages/Automation'
import { DNSPage, DNSProviderPage } from './pages/DNS'
import { UsersPage } from './pages/Users'
import { SettingsPage } from './pages/Settings'

function Root() {
  const { me, loading } = useSession()
  const setup = useSetupStatus()
  const { t } = usePrefs()
  if (setup.error) return <div className="p-6"><ErrorBanner error={setup.error} onRetry={setup.reload} /></div>
  if (loading || !setup.state) return <Spinner label={t('common.loading')} />
  if (setup.state.needs_setup) {
    return (
      <Routes>
        <Route path="*" element={<SetupPage onDone={setup.reload} />} />
      </Routes>
    )
  }
  if (!me) {
    return (
      <Routes>
        <Route path="/login" element={<LoginPage instanceName={setup.state.instance_name} />} />
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    )
  }
  if (!me.node.can_manage) {
    // Restricted follower UI: only local node status is available.
    return (
      <Routes>
        <Route element={<Layout />}>
          <Route path="*" element={<FollowerStatusPage />} />
        </Route>
      </Routes>
    )
  }
  return (
    <Routes>
      <Route path="/login" element={<Navigate to="/" replace />} />
      <Route element={<Layout />}>
        <Route index element={<OverviewPage />} />
        <Route path="monitors" element={<MonitorsPage />} />
        <Route path="monitors/new" element={<MonitorFormPage />} />
        <Route path="monitors/:id" element={<MonitorDetailPage />} />
        <Route path="monitors/:id/edit" element={<MonitorFormPage />} />
        <Route path="groups" element={<GroupsPage />} />
        <Route path="incidents" element={<IncidentsPage />} />
        <Route path="incidents/:id" element={<IncidentDetailPage />} />
        <Route path="maintenance" element={<MaintenancePage />} />
        <Route path="automation" element={<AutomationPage />} />
        <Route path="automation/new" element={<WorkflowFormPage />} />
        <Route path="automation/runs/:id" element={<RunDetailPage />} />
        <Route path="automation/:id" element={<WorkflowDetailPage />} />
        <Route path="automation/:id/edit" element={<WorkflowFormPage />} />
        <Route path="dns" element={<DNSPage />} />
        <Route path="dns/:id" element={<DNSProviderPage />} />
        <Route path="cluster" element={<ClusterPage />} />
        <Route path="audit" element={<AuditPage />} />
        <Route path="users" element={<UsersPage />} />
        <Route path="settings" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <PrefsProvider>
      <SessionProvider>
        <BrowserRouter>
          <Root />
        </BrowserRouter>
      </SessionProvider>
    </PrefsProvider>
  </StrictMode>,
)
