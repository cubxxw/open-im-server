```markdown
# open-im-server Development Patterns

> Auto-generated skill from repository analysis

## Overview
This skill teaches the core development patterns and conventions used in the `open-im-server` Go codebase. It covers file organization, import/export styles, commit message conventions, and testing patterns. By following these guidelines, contributors can maintain consistency and quality across the project.

## Coding Conventions

### File Naming
- Use **kebab-case** for all file names.
  - Example: `user-handler.go`, `message-service.go`

### Import Style
- Use **relative imports** for internal packages.
  - Example:
    ```go
    import "../utils"
    ```

### Export Style
- Use **named exports** for functions, types, and variables.
  - Example:
    ```go
    // In user-handler.go
    package user

    func HandleUserRequest() { ... }
    ```

### Commit Messages
- Follow **conventional commit** format.
- Use the `refactor` prefix for refactoring commits.
  - Example:  
    ```
    refactor: optimize message serialization logic
    ```

## Workflows

### Refactoring Code
**Trigger:** When you need to improve code structure or performance without changing external behavior  
**Command:** `/refactor`

1. Identify the code that needs refactoring.
2. Make improvements while ensuring existing functionality is preserved.
3. Run all relevant tests to confirm nothing is broken.
4. Commit changes using the conventional commit format with the `refactor` prefix.
    - Example:
      ```
      refactor: simplify user authentication flow
      ```
5. Push your changes and open a pull request for review.

## Testing Patterns

- Test files follow the pattern: `*.test.*`
  - Example: `user-handler.test.go`
- The testing framework is not explicitly specified; use Go's standard `testing` package unless otherwise noted.
  - Example:
    ```go
    import "testing"

    func TestHandleUserRequest(t *testing.T) {
        // test logic here
    }
    ```
- Place test files alongside the code they test.

## Commands
| Command     | Purpose                                      |
|-------------|----------------------------------------------|
| /refactor   | Start a code refactoring workflow            |
```
