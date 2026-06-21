---
id: daintree.recipe.run-or-create
title: Run or create Daintree workspace recipes
version: 0.1.0
summary: How to work with Daintree workspace recipes for setup and repeatable terminal layouts.
whenToUse: Use when the user asks to load, run, create, inspect, or apply a Daintree workspace recipe, or to create a worktree with a recipe.
priority: 180
risk: project
tags:
  - recipe
  - worktree
  - startup
  - workspace
  - layout
  - pr-review
  - setup
requiredTools:
  - tool.search
  - recipe.list
  - recipe.run
  - worktree.createWithRecipe
  - daintree.call
  - context.snapshot
---
Use when: the user asks about Daintree workspace recipes or creating a worktree with one.
Note: "Daintree workspace recipes" (MCP actions) are distinct from the assistant skills loaded into your context.
Procedure:
1. Inspect current Daintree context first if the project/worktree is ambiguous.
2. List available recipes with the recipe.list tool when needed.
3. To apply a recipe to an existing context, use recipe.run with the recipeId.
4. To create a new worktree with a startup recipe, use worktree.createWithRecipe.
5. Pass an idempotency requestKey for mutating calls when available.
6. These typed tools work at the operator tier; daintree.call is only the system-tier raw fallback for tools without a wrapper.
Confirmation: mutating actions require confirmation before execution.
Report back: what was started, which worktree/terminal ids were created, and whether a watcher should be attached.
