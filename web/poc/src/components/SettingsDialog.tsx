import { useState } from 'react'
import { Settings } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Separator } from '@/components/ui/separator'
import { getApiKey, setApiKey, clearApiKey, getBaseUrl, setBaseUrl } from '@/lib/settings'

interface SettingsDialogProps {
  onSettingsChange: () => void
}

export function SettingsDialog({ onSettingsChange }: SettingsDialogProps) {
  const [open, setOpen] = useState(false)
  const [apiKey, setApiKeyState] = useState('')
  const [baseUrl, setBaseUrlState] = useState('')

  function handleOpen(isOpen: boolean) {
    if (isOpen) {
      setApiKeyState(getApiKey())
      setBaseUrlState(getBaseUrl())
    }
    setOpen(isOpen)
  }

  function handleSave() {
    setApiKey(apiKey)
    setBaseUrl(baseUrl)
    setOpen(false)
    onSettingsChange()
  }

  function handleClearKey() {
    clearApiKey()
    setApiKeyState('')
    onSettingsChange()
  }

  return (
    <Dialog open={open} onOpenChange={handleOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">
          <Settings className="h-4 w-4 mr-2" />
          Settings
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Connection Settings</DialogTitle>
          <DialogDescription>
            Enter your backoffice service API key and the hub base URL.
          </DialogDescription>
        </DialogHeader>

        <div className="rounded-md bg-amber-50 border border-amber-200 p-3 text-sm text-amber-800">
          Local operator tool — you supply your own backoffice service key; it
          stays in this browser session and is never sent anywhere except the
          hub. Do not deploy this publicly.
        </div>

        <div className="grid gap-4 py-2">
          <div className="grid gap-2">
            <Label htmlFor="api-key">Service API Key</Label>
            <Input
              id="api-key"
              type="password"
              placeholder="Enter your service API key"
              value={apiKey}
              onChange={(e) => setApiKeyState(e.target.value)}
              autoComplete="off"
            />
          </div>

          <Separator />

          <div className="grid gap-2">
            <Label htmlFor="base-url">Hub Base URL</Label>
            <Input
              id="base-url"
              type="url"
              placeholder="/ (same-origin via dev proxy)"
              value={baseUrl}
              onChange={(e) => setBaseUrlState(e.target.value)}
            />
            <p className="text-xs text-slate-500">
              Leave as <code className="bg-slate-100 px-1 rounded">/</code> when
              using the Vite dev proxy (localhost:5173 → localhost:8080).
            </p>
            <p className="text-xs text-amber-700">
              Your API key is sent to whatever URL is entered here — only point
              this at your own trusted hub.
            </p>
          </div>
        </div>

        <DialogFooter className="flex-row justify-between sm:justify-between">
          <Button
            variant="outline"
            size="sm"
            onClick={handleClearKey}
            className="text-red-600 hover:text-red-700"
          >
            Clear Key
          </Button>
          <Button onClick={handleSave}>Save</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
