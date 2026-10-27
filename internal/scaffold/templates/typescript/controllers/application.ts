import { Application } from "@hotwired/stimulus"

declare global {
  interface Window {
    Stimulus: Application
  }
}

const application = Application.start()

// Set to true to log Stimulus's connections in the browser console.
application.debug = false
window.Stimulus = application

export { application }
