import { Toaster } from 'sonner'
import { Dashboard } from '@/components/Dashboard'

export default function App() {
  return (
    <>
      <Dashboard />
      <Toaster richColors position="bottom-right" />
    </>
  )
}
