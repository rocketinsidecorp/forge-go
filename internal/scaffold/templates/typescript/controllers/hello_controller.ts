import { Controller } from "@hotwired/stimulus"

// Connects to data-controller="hello". Try it in a page:
//
//   <div data-controller="hello">
//     <input data-hello-target="name" type="text">
//     <button data-action="click->hello#greet">Greet</button>
//     <span data-hello-target="output"></span>
//   </div>
export default class extends Controller {
  static targets = ["name", "output"]

  // Declared for the type checker only: Stimulus defines these properties.
  declare readonly nameTarget: HTMLInputElement
  declare readonly outputTarget: HTMLElement

  greet(): void {
    // textContent, never innerHTML: the value is user input.
    this.outputTarget.textContent = `Hello, ${this.nameTarget.value}!`
  }
}
