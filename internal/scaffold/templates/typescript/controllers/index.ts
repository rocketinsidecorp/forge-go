// Register every Stimulus controller here. To add one:
//
//   1. create app/javascript/controllers/NAME_controller.ts (copy hello_controller.ts);
//   2. import it below and register it under the name used in data-controller="NAME".
//
// Files in this directory are compiled to app/javascript/dist/controllers,
// embedded and pinned in the import map automatically (see
// app/assets/assets.go), so the specifier is the file name without its
// extension: "controllers/NAME_controller".
import { application } from "controllers/application"

import HelloController from "controllers/hello_controller"
application.register("hello", HelloController)
